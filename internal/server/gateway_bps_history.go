package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"time"
)

var errBPSHistoryMissing = errors.New("previous response is unavailable or expired; send the full conversation history")

// Only account-owned, authenticated response chains are retained. Redis values
// use the same authenticated encryption as stored account credentials. Neither
// response IDs nor prompt text are used as plaintext Redis keys.
type bpsHistoryRecord struct {
	Mode    string            `json:"mode,omitempty"`
	Owner   string            `json:"owner"`
	Session string            `json:"session"`
	Request json.RawMessage   `json:"request"`
	Output  []json.RawMessage `json:"output"`
	Expires int64             `json:"expires"`
}

type bpsMemoryHistory struct {
	mu            sync.Mutex
	items         map[string][]byte
	opaqueOwners  map[string]string
	opaqueExpires map[string]int64
	expires       map[string]int64
	order         []string
	bytes         int
}

const bpsHistoryTTL = time.Hour
const bpsHistoryBudget = 512 << 20
const bpsHistoryEntries = 32768

func bpsHistoryKey(keyID, responseID string) string {
	h := sha256.Sum256([]byte(keyID + "\x00" + responseID))
	return hex.EncodeToString(h[:])
}

func gatewayHistoryMode(ctx context.Context) (string, bool, error) {
	mode := requestInferenceMode(ctx)
	switch mode {
	case "", inferencePowerPoint:
		return inferencePowerPoint, false, nil
	case inferenceAppServer, inferenceCodexCLI:
		return mode, true, nil
	default:
		return "", false, errors.New("unsupported conversation mode")
	}
}

func (p *Proxy) bpsLoadHistory(ctx context.Context, keyID, responseID string) (bpsHistoryRecord, error) {
	var record bpsHistoryRecord
	mode, official, err := gatewayHistoryMode(ctx)
	if err != nil {
		return record, errBPSHistoryMissing
	}
	unbounded := official || gatewayUnboundedPayload(ctx)
	if responseID == "" || len(responseID) > 1024 && !unbounded {
		return record, errBPSHistoryMissing
	}
	key := bpsHistoryKey(keyID, responseID)
	var raw []byte
	var legacyDeadline int64
	if db := p.Store.DB; db != nil {
		value, deadline, err := db.bpsHistoryCipher(ctx, key)
		legacyDeadline = deadline
		if err != nil {
			return record, err
		}
		raw, err = db.Decrypt(value)
		if err != nil {
			return record, errors.New("conversation storage authentication failed")
		}
	} else {
		h := &p.BPS.history
		h.mu.Lock()
		// Saved bytes are immutable: saves replace a value and eviction only
		// drops its reference. Unmarshal below creates caller-owned fields.
		raw = h.items[key]
		h.mu.Unlock()
		if len(raw) == 0 {
			return record, errBPSHistoryMissing
		}
	}
	if !unbounded && len(raw) > maxBody || json.Unmarshal(raw, &record) != nil {
		return record, errBPSHistoryMissing
	}
	recordMode := record.Mode
	if recordMode == "" {
		recordMode = inferencePowerPoint
	}
	if recordMode != mode {
		return bpsHistoryRecord{}, errBPSHistoryMissing
	}
	if legacyDeadline != 0 && legacyDeadline <= time.Now().Unix() {
		return bpsHistoryRecord{}, errBPSHistoryMissing
	}
	if record.Expires == 0 && legacyDeadline > time.Now().Unix() {
		record.Expires = legacyDeadline
	}
	if record.Expires <= time.Now().Unix() {
		return bpsHistoryRecord{}, errBPSHistoryMissing
	}
	return record, nil
}

func (p *Proxy) bpsSaveHistory(ctx context.Context, keyID, responseID string, record bpsHistoryRecord) error {
	return p.bpsSaveHistoryRecord(ctx, keyID, responseID, record, nil)
}

// Payload limits and retention are independent: native large conversations can
// still be processed, while every mode shares the same one-hour cache budget.
func (p *Proxy) bpsSaveHistoryRecord(ctx context.Context, keyID, responseID string, record bpsHistoryRecord, proofs []string) error {
	return p.bpsSaveHistoryRecordWithinBudget(ctx, keyID, responseID, record, proofs, bpsHistoryBudget)
}

func (p *Proxy) bpsSaveHistoryRecordWithinBudget(ctx context.Context, keyID, responseID string, record bpsHistoryRecord, proofs []string, budget int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	mode, official, err := gatewayHistoryMode(ctx)
	if err != nil {
		return err
	}
	unbounded := official || gatewayUnboundedPayload(ctx)
	if responseID == "" || len(responseID) > 1024 && !unbounded {
		return errors.New("upstream response ID is missing or invalid")
	}
	record.Mode = mode
	record.Expires = time.Now().Add(bpsHistoryTTL).Unix()
	raw, err := json.Marshal(record)
	if err != nil || !unbounded && len(raw) > maxBody {
		return errors.New("conversation exceeds the bounded continuation cache; send full history")
	}
	// A response larger than the cache can still complete successfully. Keep
	// its ownership proof for client-managed full-history follow-ups, but do
	// not let one response displace the entire bounded continuation cache.
	cacheBody := len(raw)+28 <= budget
	key := bpsHistoryKey(keyID, responseID)
	if db := p.Store.DB; db != nil {
		var cipher []byte
		if cacheBody {
			cipher, err = db.Encrypt(raw)
			if err != nil {
				return errors.New("conversation encryption failed")
			}
		}
		return db.bpsStoreBoundedHistory(ctx, key, cipher, record.Expires, record.Owner, proofs, false)
	}
	h := &p.BPS.history
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	now := time.Now().Unix()
	for _, proof := range proofs {
		if h.opaqueExpires[proof] > now && h.opaqueOwners[proof] != record.Owner {
			return errBPSOpaqueOwnerConflict
		}
	}
	if cacheBody {
		if h.items == nil {
			h.items = make(map[string][]byte)
		}
		if h.expires == nil {
			h.expires = make(map[string]int64)
		}
		live := h.order[:0]
		for _, old := range h.order {
			if h.expires[old] <= now {
				h.bytes -= len(h.items[old])
				delete(h.items, old)
				delete(h.expires, old)
			} else if old != key {
				live = append(live, old)
			}
		}
		h.order = append(live, key)
		if old, ok := h.items[key]; ok {
			h.bytes -= len(old)
		}
		h.items[key], h.expires[key] = raw, record.Expires
		h.bytes += len(raw)
		for h.bytes > bpsHistoryBudget || len(h.items) > bpsHistoryEntries {
			old := h.order[0]
			h.order = h.order[1:]
			h.bytes -= len(h.items[old])
			delete(h.items, old)
			delete(h.expires, old)
		}
	}
	if h.opaqueOwners == nil {
		h.opaqueOwners = make(map[string]string)
	}
	if h.opaqueExpires == nil {
		h.opaqueExpires = make(map[string]int64)
	}
	for proof, expiry := range h.opaqueExpires {
		if expiry <= now {
			delete(h.opaqueOwners, proof)
			delete(h.opaqueExpires, proof)
		}
	}
	for _, proof := range proofs {
		h.opaqueOwners[proof], h.opaqueExpires[proof] = record.Owner, record.Expires
	}
	for len(h.opaqueOwners) > bpsOpaqueOwnerEntries {
		var oldest string
		expiry := int64(1<<63 - 1)
		for proof, candidate := range h.opaqueExpires {
			if candidate < expiry {
				oldest, expiry = proof, candidate
			}
		}
		delete(h.opaqueOwners, oldest)
		delete(h.opaqueExpires, oldest)
	}
	return nil
}

// Expand a Responses incremental request before any upstream account is used.
// New per-turn fields override the saved body; history and owner are explicit.
func (p *Proxy) bpsExpandHistory(ctx context.Context, keyID string, raw []byte, session string) ([]byte, string, string, error) {
	trimmed := bytes.TrimSpace(raw)
	if !json.Valid(raw) || len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, "", "", errors.New("request must be a JSON object")
	}
	var previous string
	if field := gatewayValidatedJSONFieldSpans(raw, "previous_response_id", "")[0]; field.end > 0 {
		if value := raw[field.start:field.end]; !bytes.Equal(value, []byte("null")) && json.Unmarshal(value, &previous) != nil {
			return nil, "", "", errors.New("previous_response_id must be a string")
		}
	}
	if previous == "" {
		return raw, session, "", nil
	}
	record, err := p.bpsLoadHistory(ctx, keyID, previous)
	if err != nil {
		return nil, "", "", err
	}
	if session != "" && record.Session != "" && session != record.Session {
		return nil, "", "", errors.New("previous response belongs to a different conversation")
	}
	// Both documents have already been fully validated, including the stored
	// request by bpsLoadHistory. Borrow their values instead of copying a whole
	// conversation into RawMessages, then copying every input item again.
	body := bpsHistoryObjectFields(raw)
	saved := bpsHistoryObjectFields(record.Request)
	if saved == nil {
		return nil, "", "", errBPSHistoryMissing
	}
	past, ok := bpsHistoryArrayContents(saved["input"])
	if !ok {
		return nil, "", "", errBPSHistoryMissing
	}
	var increment []byte
	if value := body["input"]; len(value) > 0 {
		increment, ok = bpsHistoryArrayContents(value)
		if !ok {
			return nil, "", "", errors.New("incremental input must be an array")
		}
	}
	capacity := len(past) + len(increment) + 3
	for _, item := range record.Output {
		capacity += len(item) + 1
	}
	input := make([]byte, 1, capacity)
	input[0] = '['
	appendPart := func(part []byte) {
		if len(part) == 0 {
			return
		}
		if len(input) > 1 {
			input = append(input, ',')
		}
		input = append(input, part...)
	}
	appendPart(past)
	for _, item := range record.Output {
		appendPart(item)
	}
	appendPart(increment)
	input = append(input, ']')
	if len(input) == 2 && bytes.Equal(bytes.TrimSpace(saved["input"]), []byte("null")) {
		input = []byte("null")
	}
	for name, value := range body {
		saved[name] = value
	}
	delete(saved, "previous_response_id")
	saved["input"] = input
	encoded, err := json.Marshal(saved)
	_, official, modeErr := gatewayHistoryMode(ctx)
	if err != nil || modeErr != nil || !official && !gatewayUnboundedPayload(ctx) && len(encoded) > maxBody {
		return nil, "", "", errors.New("expanded conversation exceeds request limit")
	}
	return encoded, record.Session, record.Owner, nil
}

// raw must already be valid JSON. Returned values borrow immutable input bytes;
// escaped/duplicate keys retain encoding/json's decoded, final-value semantics.
func bpsHistoryObjectFields(raw []byte) map[string]json.RawMessage {
	fields, _ := bpsHistoryObjectFieldsReusable(raw)
	return fields
}

// Reuse requires unique, unescaped top-level names: encoding a map would
// otherwise remove duplicate members or shorten escaped names, changing the
// serialized cache size even when the decoded request has the same meaning.
func bpsHistoryObjectFieldsReusable(raw []byte) (map[string]json.RawMessage, bool) {
	position := gatewayJSONSkipSpace(raw, 0)
	if position >= len(raw) || raw[position] != '{' {
		return nil, false
	}
	fields := make(map[string]json.RawMessage)
	reusable := true
	position = gatewayJSONSkipSpace(raw, position+1)
	for raw[position] != '}' {
		end := gatewayJSONStringEnd(raw, position)
		if bytes.IndexByte(raw[position:end], '\\') >= 0 {
			reusable = false
		}
		var name string
		_ = json.Unmarshal(raw[position:end], &name)
		position = gatewayJSONSkipSpace(raw, gatewayJSONSkipSpace(raw, end)+1)
		end = gatewayJSONValueEnd(raw, position)
		if _, exists := fields[name]; exists {
			reusable = false
		}
		fields[name] = raw[position:end]
		position = gatewayJSONSkipSpace(raw, end)
		if raw[position] == ',' {
			position = gatewayJSONSkipSpace(raw, position+1)
		}
	}
	return fields, reusable
}

func bpsHistoryArrayContents(raw []byte) ([]byte, bool) {
	raw = bytes.TrimSpace(raw)
	if bytes.Equal(raw, []byte("null")) {
		return nil, true
	}
	if len(raw) < 2 || raw[0] != '[' {
		return nil, false
	}
	return bytes.TrimSpace(raw[1 : len(raw)-1]), true
}
