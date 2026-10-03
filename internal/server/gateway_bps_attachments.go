package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"sync"
	"time"

	"codex-agent/internal/bps"
)

const bpsAttachmentTTL = time.Hour

// Successful references expire at their individual TTL even between sweeps.
// Full-map cleanup is opportunistic; no background worker or size cap is needed.
const bpsAttachmentSweepInterval = time.Minute

type bpsAttachmentError struct {
	StatusCode int
	Stage      string
}

func (e *bpsAttachmentError) Error() string {
	switch e.Stage {
	case "image":
		return "invalid inline image: expected valid base64 PNG, JPEG, GIF or WebP data"
	case "response":
		return "upstream image upload returned no valid file reference"
	case "upload":
		return fmt.Sprintf("upstream image upload returned HTTP %d", e.StatusCode)
	default:
		return "cannot prepare image for the selected upstream"
	}
}

type bpsAttachmentEntry struct {
	fileID  string
	expires time.Time
}

type bpsAttachmentFlight struct {
	done   chan struct{}
	fileID string
	err    error
}

// Only uploaded file references are retained. Neither image bytes, request
// history, bearer tokens nor credentials survive a prepare call in this cache.
type bpsAttachmentCache struct {
	mu        sync.Mutex
	entries   map[string]bpsAttachmentEntry
	pending   map[string]*bpsAttachmentFlight
	nextSweep time.Time
}

func (c *bpsAttachmentCache) file(ctx context.Context, key string, upload func() (string, error)) (string, error) {
	return c.fileAttempt(ctx, key, upload, true)
}

func (c *bpsAttachmentCache) fileAttempt(ctx context.Context, key string, upload func() (string, error), retryCanceledLeader bool) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	c.mu.Lock()
	now := time.Now()
	if !now.Before(c.nextSweep) {
		for cachedKey, entry := range c.entries {
			if !entry.expires.After(now) {
				delete(c.entries, cachedKey)
			}
		}
		c.nextSweep = now.Add(bpsAttachmentSweepInterval)
	}
	if entry, ok := c.entries[key]; ok {
		if entry.expires.After(now) {
			c.mu.Unlock()
			return entry.fileID, nil
		}
		// Individual expiry is authoritative; deferred sweeping never extends
		// this reference's validity or renews the TTL on a cache hit.
		delete(c.entries, key)
	}
	if flight := c.pending[key]; flight != nil {
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-flight.done:
			if err := ctx.Err(); err != nil {
				return "", err
			}
			// A different request owned this upload. Its cancellation must not
			// cancel a healthy waiter. Rejoin or lead once; HTTP errors never retry.
			if retryCanceledLeader && (errors.Is(flight.err, context.Canceled) || errors.Is(flight.err, context.DeadlineExceeded)) {
				return c.fileAttempt(ctx, key, upload, false)
			}
			return flight.fileID, flight.err
		}
	}
	if c.pending == nil {
		c.pending = make(map[string]*bpsAttachmentFlight)
	}
	flight := &bpsAttachmentFlight{done: make(chan struct{})}
	c.pending[key] = flight
	c.mu.Unlock()
	fileID, err := upload()
	c.mu.Lock()
	if err == nil {
		if c.entries == nil {
			c.entries = make(map[string]bpsAttachmentEntry)
		}
		c.entries[key] = bpsAttachmentEntry{fileID: fileID, expires: time.Now().Add(bpsAttachmentTTL)}
	}
	flight.fileID, flight.err = fileID, err
	delete(c.pending, key)
	close(flight.done)
	c.mu.Unlock()
	return fileID, err
}

type bpsInlineImage struct {
	item, part int
	field      string
	mime, ext  string
	data       []byte
}

func bpsDecodeInlineImage(uri string) (string, string, []byte, error) {
	invalid := func() (string, string, []byte, error) {
		return "", "", nil, &bpsAttachmentError{StatusCode: http.StatusBadRequest, Stage: "image"}
	}
	header, encoded, found := strings.Cut(uri, ",")
	if !found || !strings.HasPrefix(header, "data:") || !strings.HasSuffix(header, ";base64") {
		return invalid()
	}
	mediaType := strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(header, "data:"), ";base64"))
	ext := map[string]string{"image/png": "png", "image/jpeg": "jpg", "image/gif": "gif", "image/webp": "webp"}[mediaType]
	if ext == "" {
		return invalid()
	}
	data, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || len(data) == 0 {
		return invalid()
	}
	valid := false
	switch mediaType {
	case "image/png":
		valid = bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n"))
	case "image/jpeg":
		valid = bytes.HasPrefix(data, []byte{0xff, 0xd8, 0xff})
	case "image/gif":
		valid = bytes.HasPrefix(data, []byte("GIF87a")) || bytes.HasPrefix(data, []byte("GIF89a"))
	case "image/webp":
		valid = len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP"
	}
	if !valid {
		return invalid()
	}
	return mediaType, ext, data, nil
}

func bpsAttachmentCacheKey(keyID string, account Account, proxy *ProxyEntry, baseURL, mediaType string, data []byte) string {
	imageHash := sha256.Sum256(data)
	proxyHash := "direct"
	if proxy != nil {
		fp := gatewayProxyFingerprint(*proxy)
		proxyHash = proxy.ID + ":" + hex.EncodeToString(fp[:])
	}
	encoded, _ := json.Marshal([]string{"bps-attachment-v1", keyID, gatewayAccountRef(account), strings.TrimRight(baseURL, "/"), proxyHash, mediaType, hex.EncodeToString(imageHash[:])})
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

// The upstream's user-image writer uploads bytes and sends a file_id; it rejects
// inline image_url data URIs in user messages and custom tool output arrays.
// Function tool outputs keep their separate existing protocol. Prepare changes
// only this outbound copy, never saved history or the original image bytes.
func (p *Proxy) prepareBPSAttachments(ctx context.Context, account Account, proxy *ProxyEntry, keyID string, prepared *bps.Request, settings Settings, authorize ...func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if prepared == nil || p.BPS == nil || (!bytes.Contains(prepared.Body, []byte(`"image_url"`)) && !bytes.Contains(prepared.Body, []byte(`\u`))) {
		return nil
	}
	var body map[string]json.RawMessage
	if json.Unmarshal(prepared.Body, &body) != nil {
		return &bpsAttachmentError{StatusCode: http.StatusBadRequest, Stage: "request"}
	}
	var items []json.RawMessage
	if json.Unmarshal(body["input"], &items) != nil {
		return &bpsAttachmentError{StatusCode: http.StatusBadRequest, Stage: "request"}
	}
	var images []bpsInlineImage
	for itemIndex, raw := range items {
		var item map[string]json.RawMessage
		if json.Unmarshal(raw, &item) != nil {
			continue
		}
		kind := bpsAttachmentString(item["type"])
		field := "content"
		switch {
		case kind == "custom_tool_call_output":
			field = "output"
		case kind == "message" || len(item["type"]) == 0:
			role := bpsAttachmentString(item["role"])
			if role != "user" && role != "developer" && role != "system" {
				continue
			}
		default:
			continue
		}
		var content []json.RawMessage
		if json.Unmarshal(item[field], &content) != nil {
			continue
		}
		for partIndex, raw := range content {
			var part map[string]json.RawMessage
			if json.Unmarshal(raw, &part) != nil || bpsAttachmentString(part["type"]) != "input_image" {
				continue
			}
			if bpsAttachmentString(part["file_id"]) != "" {
				continue
			}
			value, exists := part["image_url"]
			if !exists {
				continue
			}
			var uri string
			if json.Unmarshal(value, &uri) != nil {
				return &bpsAttachmentError{StatusCode: http.StatusBadRequest, Stage: "image"}
			}
			// Remote URLs retain their existing upstream behavior. This gateway
			// never fetches an arbitrary caller-provided URL to upload it.
			if !strings.HasPrefix(uri, "data:") {
				continue
			}
			mediaType, ext, data, err := bpsDecodeInlineImage(uri)
			if err != nil {
				return err
			}
			images = append(images, bpsInlineImage{item: itemIndex, part: partIndex, field: field, mime: mediaType, ext: ext, data: data})
		}
	}
	if len(images) == 0 {
		return nil
	}
	checkAccess := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		for _, check := range authorize {
			if check != nil {
				if err := check(ctx); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := checkAccess(); err != nil {
		return err
	}
	owner := gatewayAccountRef(account)
	current, err := p.gatewayCurrentBoundAccountContext(ctx, account, owner)
	if err != nil {
		return err
	}
	for _, image := range images {
		if err := checkAccess(); err != nil {
			return err
		}
		cacheKey := bpsAttachmentCacheKey(keyID, current, proxy, p.BaseURL, image.mime, image.data)
		fileID, err := p.BPS.attachments.file(ctx, cacheKey, func() (string, error) {
			return p.uploadBPSAttachment(ctx, current, proxy, owner, image, settings, checkAccess)
		})
		if err != nil {
			return err
		}
		if err := checkAccess(); err != nil {
			return err
		}
		var item, part map[string]json.RawMessage
		var content []json.RawMessage
		_ = json.Unmarshal(items[image.item], &item)
		_ = json.Unmarshal(item[image.field], &content)
		_ = json.Unmarshal(content[image.part], &part)
		delete(part, "image_url")
		part["file_id"], _ = json.Marshal(fileID)
		if value := part["detail"]; len(value) == 0 || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			part["detail"] = json.RawMessage(`"auto"`)
		}
		content[image.part], _ = json.Marshal(part)
		item[image.field], _ = json.Marshal(content)
		items[image.item], _ = json.Marshal(item)
	}
	if _, err := p.gatewayCurrentBoundAccountContext(ctx, current, owner); err != nil {
		return err
	}
	body["input"], _ = json.Marshal(items)
	out, err := json.Marshal(body)
	if err != nil {
		return err
	}
	prepared.Body = out
	prepared.Adaptations = append(prepared.Adaptations, "inline image -> account attachment")
	return nil
}

func bpsAttachmentString(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}

func (p *Proxy) uploadBPSAttachment(ctx context.Context, account Account, proxy *ProxyEntry, owner string, image bpsInlineImage, settings Settings, checkAccess func() error) (string, error) {
	current, err := p.gatewayCurrentBoundAccountContext(ctx, account, owner)
	if err != nil {
		return "", err
	}
	target, err := gatewayUpstreamURL(p.BaseURL, "/attachments", nil)
	if err != nil {
		return "", err
	}
	var framing bytes.Buffer
	writer := multipart.NewWriter(&framing)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": "file", "filename": "image." + image.ext}))
	header.Set("Content-Type", image.mime)
	if _, err = writer.CreatePart(header); err != nil {
		return "", err
	}
	prefixBytes := framing.Len()
	if err = writer.Close(); err != nil {
		return "", err
	}
	frame := framing.Bytes()
	reader := io.MultiReader(bytes.NewReader(frame[:prefixBytes]), bytes.NewReader(image.data), bytes.NewReader(frame[prefixBytes:]))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, reader)
	if err != nil {
		return "", err
	}
	req.ContentLength = int64(len(frame)) + int64(len(image.data))
	req.GetBody = nil // uploading a file is not an automatically replayable POST
	applyBPSHeaders(req.Header, current)
	req.Header.Set("Accept", "application/json")
	client := p.gatewayRequestClientWithHeaderTimeout(ctx, proxy, gatewayClientHTTP, time.Duration(settings.UpstreamResponseHeaderTimeoutSeconds)*time.Second)
	var observation RequestRecord
	res, err := p.doObservedAccountRequest(&observation, client, req, current, func(client *http.Client, request *http.Request) (*http.Response, error) {
		if err := checkAccess(); err != nil {
			return nil, err
		}
		// doAccountRequest applies the credential-epoch identity headers before
		// dispatch, including JSON Content-Type. Override only the multipart type.
		request.Header.Set("Content-Type", writer.FormDataContentType())
		return doStreamingRequestWithIdleTimeout(client, request, time.Duration(settings.UpstreamStreamIdleTimeoutSeconds)*time.Second)
	}, nil)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		// Do not expose upstream payloads containing file metadata or credentials.
		_, _ = io.Copy(io.Discard, res.Body)
		return "", &bpsAttachmentError{StatusCode: res.StatusCode, Stage: "upload"}
	}
	var reply struct {
		FileID string `json:"openai_file_id"`
	}
	decoder := json.NewDecoder(res.Body)
	if err = decoder.Decode(&reply); err != nil || strings.TrimSpace(reply.FileID) == "" {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		return "", &bpsAttachmentError{StatusCode: http.StatusBadGateway, Stage: "response"}
	}
	var trailing any
	if err = decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		return "", &bpsAttachmentError{StatusCode: http.StatusBadGateway, Stage: "response"}
	}
	if _, err = p.gatewayCurrentBoundAccountContext(ctx, current, owner); err != nil {
		return "", err
	}
	if err := checkAccess(); err != nil {
		return "", err
	}
	return strings.TrimSpace(reply.FileID), nil
}
