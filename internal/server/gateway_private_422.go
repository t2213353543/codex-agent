package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const private422TTL = 7 * 24 * time.Hour

// Taking the arm and installing a unique claim is one operation. A response
// reader may outlive a short lock TTL: do not make the arm available again.
const private422ClaimScript = `if redis.call('GET',KEYS[1])~=ARGV[1] or redis.call('EXISTS',KEYS[2])~=0 then return 0 end
redis.call('SET',KEYS[2],ARGV[2],'EX',ARGV[4]);redis.call('SET',KEYS[3],ARGV[3],'EX',ARGV[4]);redis.call('DEL',KEYS[1]);return 1`

const private422StoreScript = `if redis.call('GET',KEYS[1])~=ARGV[1] then return 0 end
redis.call('SET',KEYS[2],ARGV[2],'EX',ARGV[4]);redis.call('SET',KEYS[3],ARGV[3],'EX',ARGV[4]);redis.call('SET',KEYS[1],'completed','EX',ARGV[4]);return 1`

const private422FailScript = `if redis.call('GET',KEYS[1])~=ARGV[1] then return 0 end
redis.call('SET',KEYS[2],ARGV[2],'EX',ARGV[3]);redis.call('SET',KEYS[1],'failed','EX',ARGV[3]);return 1`

var private422BinaryIdentity struct {
	once sync.Once
	sha  string
	err  error
}

type private422Arm struct {
	ID        string `json:"capture_id"`
	BinarySHA string `json:"binary_sha256"`
}

type private422Capture struct {
	Schema            string      `json:"schema"`
	ID                string      `json:"capture_id"`
	CapturedAt        int64       `json:"captured_at"`
	BinarySHA         string      `json:"binary_sha256"`
	AccountID         string      `json:"account_id"`
	OwnerSHA          string      `json:"owner_sha256"`
	CredentialVersion int64       `json:"credential_version"`
	AccessTokenSHA    string      `json:"access_token_sha256"`
	KeyID             string      `json:"key_id"`
	ProxyID           string      `json:"proxy_id,omitempty"`
	ProxySHA          string      `json:"proxy_sha256,omitempty"`
	Method            string      `json:"method"`
	URL               string      `json:"url"`
	RequestHeaders    http.Header `json:"request_headers"`
	Request           []byte      `json:"request_body"`
	RequestSHA        string      `json:"request_sha256"`
	ResponseHeaders   http.Header `json:"response_headers"`
	Response          []byte      `json:"response_body"`
	ResponseSHA       string      `json:"response_sha256"`
	Status            int         `json:"status"`
	ResponseComplete  bool        `json:"response_complete"`
}

func private422SHA(raw []byte) string {
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func private422ValidID(id string) bool {
	raw, err := hex.DecodeString(id)
	return err == nil && len(raw) == 16 && strings.ToLower(id) == id
}

func private422BinarySHA() (string, error) {
	private422BinaryIdentity.once.Do(func() {
		private422BinaryIdentity.sha, private422BinaryIdentity.err = private422HashExecutable()
	})
	return private422BinaryIdentity.sha, private422BinaryIdentity.err
}

func private422HashExecutable() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", err
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	digest := sha256.New()
	if _, err = io.Copy(digest, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func private422Headers(source http.Header) http.Header {
	// Explicit protocol headers only. Never copy arbitrary client headers,
	// cookies, authentication headers or future credential-bearing headers.
	out := make(http.Header)
	for _, name := range []string{"Content-Type", "Content-Encoding", "Accept", "User-Agent", "Origin", "Referer", "X-Request-Id", "Openai-Request-Id",
		"X-Basispoints-Auth-Mode", "X-Openai-Internal-Basispoints-Client-Product", "X-Openai-Internal-Basispoints-Client-Platform",
		"X-Openai-Internal-Basispoints-Client-Agent-Profile", "X-Openai-Internal-Basispoints-Client-Editor", "X-Openai-Internal-Basispoints-Client-Host",
		"X-Openai-Internal-Basispoints-Client-Runtime", "X-Openai-Internal-Basispoints-Client-Platform-Class"} {
		if values := source.Values(name); len(values) > 0 {
			out[name] = append([]string(nil), values...)
		}
	}
	return out
}

// Capture only the next explicitly armed, actual BPS HTTP 422. The normal
// error reader consumes the response: this hook does not pre-read, cancel,
// close or replace its original errors. No admin API or replay is installed.
func (p *Proxy) capturePrivate422(req *http.Request, res *http.Response, account Account, proxy *ProxyEntry, keyID string) {
	if p == nil || p.Store.DB == nil || p.Store.DB.Redis == nil || req == nil || req.URL == nil || req.GetBody == nil || res == nil || res.Body == nil || res.StatusCode != http.StatusUnprocessableEntity || req.Method != http.MethodPost {
		return
	}
	if req.URL.Path != "/basispoints/api/responses" && req.URL.Path != "/basispoints/api/responses/compact" {
		return
	}
	db := p.Store.DB
	prefix := db.Namespace + "diagnostic:inference422:"
	ctx, cancel := context.WithTimeout(req.Context(), 250*time.Millisecond)
	defer cancel()
	rawArm, err := db.Redis.Get(ctx, prefix+"armed").Bytes()
	if err != nil {
		return
	}
	var arm private422Arm
	if json.Unmarshal(rawArm, &arm) != nil || !private422ValidID(arm.ID) || len(arm.BinarySHA) != 64 {
		return
	}
	binarySHA, err := private422BinarySHA()
	if err != nil || binarySHA != arm.BinarySHA {
		return
	}
	credential, ok := req.Context().Value(gatewayRequestCredentialsKey{}).(gatewayRequestCredentials)
	if !ok || credential.id == "" || credential.version < 1 || bearerToken(req) == "" {
		return
	}
	var random [16]byte
	if _, err = rand.Read(random[:]); err != nil {
		return
	}
	claimToken := hex.EncodeToString(random[:])
	capturedAt := time.Now().Unix()
	metadata, _ := json.Marshal(map[string]any{"capture_id": arm.ID, "status": "capturing", "captured_at": capturedAt, "binary_sha256": arm.BinarySHA})
	claimKey := prefix + "claim:" + arm.ID
	metadataKey := prefix + "metadata:" + arm.ID
	claimed, err := db.Redis.Eval(ctx, private422ClaimScript, []string{prefix + "armed", claimKey, metadataKey}, string(rawArm), claimToken, metadata, int64(private422TTL/time.Second)).Int()
	if err != nil || claimed != 1 {
		return
	}
	fail := func(stage string) {
		// No exception/body text is stored. A failed one-shot is terminal and
		// must not re-arm against a different user's later request.
		metadata, _ := json.Marshal(map[string]any{"capture_id": arm.ID, "status": "capture_failed", "stage": stage, "captured_at": capturedAt, "binary_sha256": arm.BinarySHA})
		failureCtx, stop := context.WithTimeout(context.WithoutCancel(req.Context()), 250*time.Millisecond)
		defer stop()
		_ = db.Redis.Eval(failureCtx, private422FailScript, []string{claimKey, metadataKey}, claimToken, metadata, int64(private422TTL/time.Second)).Err()
	}
	reader, err := req.GetBody()
	if err != nil {
		fail("request_body_open")
		return
	}
	requestBody, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil {
		fail("request_body_read")
		return
	}
	capture := private422Capture{Schema: "private-inference-422-v2", ID: arm.ID, CapturedAt: capturedAt, BinarySHA: arm.BinarySHA,
		AccountID: credential.id, OwnerSHA: private422SHA([]byte(gatewayAccountRef(account))), CredentialVersion: credential.version,
		AccessTokenSHA: private422SHA([]byte(bearerToken(req))), KeyID: keyID, Method: req.Method, URL: req.URL.String(),
		RequestHeaders: private422Headers(req.Header), Request: requestBody, RequestSHA: private422SHA(requestBody),
		ResponseHeaders: private422Headers(res.Header), Status: res.StatusCode}
	if proxy != nil {
		capture.ProxyID = proxy.ID
		fingerprint := gatewayProxyFingerprint(*proxy)
		capture.ProxySHA = hex.EncodeToString(fingerprint[:])
	}
	res.Body = &private422ResponseBody{ReadCloser: res.Body, complete: func(raw []byte, complete bool) {
		capture.Response, capture.ResponseSHA, capture.ResponseComplete = raw, private422SHA(raw), complete
		encoded, encodeErr := json.Marshal(capture)
		if encodeErr != nil {
			fail("encode")
			return
		}
		sealed, sealErr := db.Encrypt(encoded)
		if sealErr != nil {
			fail("encrypt")
			return
		}
		metadata, _ := json.Marshal(map[string]any{"capture_id": arm.ID, "status": "captured", "captured_at": capturedAt, "request_bytes": len(requestBody), "response_bytes": len(raw), "response_complete": complete, "binary_sha256": arm.BinarySHA})
		storeCtx, stop := context.WithTimeout(context.WithoutCancel(req.Context()), 250*time.Millisecond)
		defer stop()
		if err := db.Redis.Eval(storeCtx, private422StoreScript, []string{claimKey, prefix + "capture:" + arm.ID, metadataKey}, claimToken, sealed, metadata, int64(private422TTL/time.Second)).Err(); err != nil {
			fail("persist")
		}
	}}
}

type private422ResponseBody struct {
	io.ReadCloser
	mu       sync.Mutex
	raw      []byte
	done     bool
	complete func([]byte, bool)
}

func (b *private422ResponseBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.mu.Lock()
	if !b.done {
		b.raw = append(b.raw, p[:n]...)
	}
	var raw []byte
	finish := !b.done && err != nil
	if finish {
		b.done, raw, b.raw = true, b.raw, nil
	}
	b.mu.Unlock()
	if finish {
		b.complete(raw, err == io.EOF)
	}
	return n, err
}

func (b *private422ResponseBody) Close() error {
	err := b.ReadCloser.Close()
	b.mu.Lock()
	finish := !b.done
	raw := b.raw
	b.done, b.raw = true, nil
	b.mu.Unlock()
	if finish {
		b.complete(raw, false)
	}
	return err
}
