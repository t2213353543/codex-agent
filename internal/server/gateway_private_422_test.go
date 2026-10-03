package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type private422FixtureReader struct {
	err error
}

func (r private422FixtureReader) Read([]byte) (int, error) { return 0, r.err }
func (r private422FixtureReader) Close() error             { return r.err }

func TestPrivate422ReaderPreservesErrorAndClose(t *testing.T) {
	sentinel := errors.New("synthetic reader error")
	for _, terminal := range []error{io.EOF, sentinel} {
		calls := 0
		var captured []byte
		var complete bool
		underlying := io.NopCloser(io.MultiReader(bytes.NewReader([]byte("synthetic bytes")), private422FixtureReader{terminal}))
		body := &private422ResponseBody{ReadCloser: underlying, complete: func(raw []byte, done bool) { calls++; captured = raw; complete = done }}
		raw, err := io.ReadAll(body)
		if !bytes.Equal(raw, []byte("synthetic bytes")) || (terminal == sentinel && !errors.Is(err, sentinel)) || (terminal == io.EOF && err != nil) {
			t.Fatal("response bytes or original error changed")
		}
		_ = body.Close()
		if calls != 1 || !bytes.Equal(raw, captured) || complete != (terminal == io.EOF) {
			t.Fatal("capture completion does not match actual response consumption")
		}
	}
	calls := 0
	body := &private422ResponseBody{ReadCloser: private422FixtureReader{sentinel}, complete: func(raw []byte, complete bool) {
		calls++
		if complete || len(raw) != 0 {
			t.Error("early close reported a complete response")
		}
	}}
	if err := body.Close(); !errors.Is(err, sentinel) || calls != 1 {
		t.Fatal("original close error changed")
	}
}

func private422FixtureKeys(f *bpsRedisReviewFixture, p *Proxy, id string) (string, string, string, string) {
	prefix := p.Store.DB.Namespace + "diagnostic:inference422:"
	return f.track(prefix + "armed"), f.track(prefix + "claim:" + id), f.track(prefix + "capture:" + id), f.track(prefix + "metadata:" + id)
}

func TestPrivate422RedisFullCaptureExactEncryptedAndOneShot(t *testing.T) {
	f := newBPSRedisReviewFixture(t)
	p := f.proxy()
	db := p.Store.DB
	ctx := context.Background()
	id := "0123456789abcdef0123456789abcdef"
	armed, claim, cipherKey, metadataKey := private422FixtureKeys(f, p, id)
	binarySHA, err := private422BinarySHA()
	if err != nil {
		t.Fatal(err)
	}
	arm, _ := json.Marshal(private422Arm{ID: id, BinarySHA: binarySHA})
	if err = db.Redis.Set(ctx, armed, arm, private422TTL).Err(); err != nil {
		t.Fatal(err)
	}
	requestBytes := []byte(`{"input":[{"role":"user","content":"PRIVATE_PROMPT"}],"integer":9007199254740993}`)
	responseBytes := []byte(`{"detail":[{"loc":["body","input",1,"encrypted_content"],"msg":"PRIVATE_MESSAGE","input":"PRIVATE_VALUE"}]}`)
	account := Account{ID: "synthetic-a", AccountID: "synthetic-workspace", AccessToken: "PRIVATE_TOKEN", CredentialVersion: 3, Enabled: true, Identity: &managedAccountIdentity{UserID: "synthetic-user"}}
	proxy := &ProxyEntry{ID: "synthetic-proxy"}
	req, err := http.NewRequestWithContext(withGatewayRequestCredentials(ctx, account), http.MethodPost, "https://bps.openai.com/basispoints/api/responses", bytes.NewReader(requestBytes))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer PRIVATE_TOKEN")
	req.Header.Set("Cookie", "PRIVATE_COOKIE")
	req.Header.Set("X-Api-Key", "PRIVATE_KEY")
	req.Header.Set("Content-Type", "application/json")
	res := &http.Response{StatusCode: 422, Body: io.NopCloser(bytes.NewReader(responseBytes)), Header: http.Header{"Set-Cookie": {"PRIVATE_SET_COOKIE"}}}
	p.capturePrivate422(req, res, account, proxy, "synthetic-key")
	if _, ok := res.Body.(*private422ResponseBody); !ok {
		t.Fatal("armed capture was not installed")
	}
	if count, err := db.Redis.Exists(ctx, armed).Result(); err != nil || count != 0 {
		t.Fatal("arm not consumed atomically")
	}
	if count, err := db.Redis.Exists(ctx, cipherKey).Result(); err != nil || count != 0 {
		t.Fatal("response captured before normal reader consumed it")
	}
	raw, err := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if err != nil || !bytes.Equal(raw, responseBytes) {
		t.Fatal("capture altered response")
	}
	sealed, err := db.Redis.Get(ctx, cipherKey).Bytes()
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"PRIVATE_PROMPT", "PRIVATE_TOKEN", "PRIVATE_MESSAGE", "PRIVATE_VALUE"} {
		if bytes.Contains(sealed, []byte(secret)) {
			t.Fatal("plaintext leaked into Redis capture")
		}
	}
	plain, err := db.Decrypt(sealed)
	if err != nil {
		t.Fatal(err)
	}
	var captured private422Capture
	if err = json.Unmarshal(plain, &captured); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(captured.Request, requestBytes) || !bytes.Equal(captured.Response, responseBytes) || !captured.ResponseComplete || captured.AccountID != account.ID || captured.CredentialVersion != 3 || captured.OwnerSHA != private422SHA([]byte(gatewayAccountRef(account))) || captured.ProxyID != proxy.ID || captured.AccessTokenSHA != private422SHA([]byte(account.AccessToken)) {
		t.Fatal("exact body, owner, credential or proxy evidence changed")
	}
	for _, secret := range []string{"PRIVATE_TOKEN", "PRIVATE_COOKIE", "PRIVATE_KEY", "PRIVATE_SET_COOKIE"} {
		if bytes.Contains(plain, []byte(secret)) {
			t.Fatal("transport credential retained in envelope")
		}
	}
	for _, key := range []string{claim, cipherKey, metadataKey} {
		if ttl, err := db.Redis.TTL(ctx, key).Result(); err != nil || ttl <= 0 || ttl > private422TTL {
			t.Fatal("capture key has missing/unbounded TTL")
		}
	}
	sealed[len(sealed)-1] ^= 1
	if _, err = db.Decrypt(sealed); err == nil {
		t.Fatal("tampered capture decrypted")
	}
	second := &http.Response{StatusCode: 422, Body: io.NopCloser(bytes.NewReader([]byte("next")))}
	original := second.Body
	p.capturePrivate422(req, second, account, proxy, "synthetic-key")
	if second.Body != original {
		t.Fatal("one-shot capture intercepted a second request")
	}
}

func TestPrivate422RedisConcurrentClaimAndWrongOwnerCannotOverwrite(t *testing.T) {
	f := newBPSRedisReviewFixture(t)
	p := f.proxy()
	db := p.Store.DB
	ctx := context.Background()
	id := "fedcba9876543210fedcba9876543210"
	armed, claim, cipherKey, metadataKey := private422FixtureKeys(f, p, id)
	const arm = `{"synthetic":"arm"}`
	if err := db.Redis.Set(ctx, armed, arm, private422TTL).Err(); err != nil {
		t.Fatal(err)
	}
	var successes atomic.Int32
	var group sync.WaitGroup
	var winner string
	var mu sync.Mutex
	for i := 0; i < 24; i++ {
		group.Go(func() {
			token := newID("claim-")
			value, err := db.Redis.Eval(ctx, private422ClaimScript, []string{armed, claim, metadataKey}, arm, token, `{}`, int64(private422TTL/time.Second)).Int()
			if err != nil {
				t.Error("claim script failed")
				return
			}
			if value == 1 {
				successes.Add(1)
				mu.Lock()
				winner = token
				mu.Unlock()
			}
		})
	}
	group.Wait()
	if successes.Load() != 1 {
		t.Fatal("concurrent 422s did not produce exactly one claim")
	}
	args := []string{claim, cipherKey, metadataKey}
	if value, err := db.Redis.Eval(ctx, private422StoreScript, args, "stale-claim", "synthetic-ciphertext", `{}`, int64(private422TTL/time.Second)).Int(); err != nil || value != 0 {
		t.Fatal("wrong capture owner wrote result")
	}
	if count, err := db.Redis.Exists(ctx, cipherKey).Result(); err != nil || count != 0 {
		t.Fatal("wrong claim created partial ciphertext")
	}
	if value, err := db.Redis.Eval(ctx, private422StoreScript, args, winner, "synthetic-ciphertext", `{"status":"captured"}`, int64(private422TTL/time.Second)).Int(); err != nil || value != 1 {
		t.Fatal("winning claim did not persist atomically")
	}
	if value, err := db.Redis.Eval(ctx, private422StoreScript, args, winner, "overwritten", `{}`, int64(private422TTL/time.Second)).Int(); err != nil || value != 0 {
		t.Fatal("completed capture was overwritten")
	}
	if value, err := db.Redis.Get(ctx, cipherKey).Result(); err != nil || value != "synthetic-ciphertext" {
		t.Fatal("immutable capture changed")
	}
}

func TestPrivate422DisabledAndSuccessDoNotReadResponse(t *testing.T) {
	p := &Proxy{}
	request, _ := http.NewRequest(http.MethodPost, "https://bps.openai.com/basispoints/api/responses", bytes.NewReader([]byte(`{}`)))
	for _, status := range []int{200, 422} {
		body := io.NopCloser(bytes.NewReader([]byte("original")))
		res := &http.Response{StatusCode: status, Body: body}
		p.capturePrivate422(request, res, Account{}, nil, "")
		if res.Body != body {
			t.Fatal("disabled capture changed response body")
		}
	}
}

func TestPrivate422RedisCaptureFailureIsTerminalAndPreservesOriginal(t *testing.T) {
	f := newBPSRedisReviewFixture(t)
	p := f.proxy()
	db := p.Store.DB
	ctx := context.Background()
	id := "1234567890abcdef1234567890abcdef"
	armed, claim, cipherKey, metadataKey := private422FixtureKeys(f, p, id)
	binarySHA, err := private422BinarySHA()
	if err != nil {
		t.Fatal(err)
	}
	arm, _ := json.Marshal(private422Arm{ID: id, BinarySHA: binarySHA})
	if err = db.Redis.Set(ctx, armed, arm, private422TTL).Err(); err != nil {
		t.Fatal(err)
	}
	account := Account{ID: "synthetic-failure", AccessToken: "synthetic-token", CredentialVersion: 2}
	req, _ := http.NewRequestWithContext(withGatewayRequestCredentials(ctx, account), http.MethodPost, "https://bps.openai.com/basispoints/api/responses", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Authorization", "Bearer synthetic-token")
	reads := 0
	req.GetBody = func() (io.ReadCloser, error) { reads++; return nil, errors.New("PRIVATE_OPEN_ERROR") }
	body := io.NopCloser(bytes.NewReader([]byte("original error response")))
	res := &http.Response{StatusCode: 422, Body: body}
	p.capturePrivate422(req, res, account, nil, "synthetic-key")
	if reads != 1 || res.Body != body {
		t.Fatal("failed capture changed original response")
	}
	if value, err := db.Redis.Get(ctx, claim).Result(); err != nil || value != "failed" {
		t.Fatal("failed capture did not leave terminal claim")
	}
	if count, err := db.Redis.Exists(ctx, armed, cipherKey).Result(); err != nil || count != 0 {
		t.Fatal("failed capture re-armed or stored partial ciphertext")
	}
	metadata, err := db.Redis.Get(ctx, metadataKey).Bytes()
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	if json.Unmarshal(metadata, &state) != nil || state["status"] != "capture_failed" || state["stage"] != "request_body_open" || bytes.Contains(metadata, []byte("PRIVATE_OPEN_ERROR")) {
		t.Fatal("capture failure leaked text or lacked explicit state")
	}
	p.capturePrivate422(req, res, account, nil, "synthetic-key")
	if reads != 1 {
		t.Fatal("unarmed second failure copied request body")
	}
}
