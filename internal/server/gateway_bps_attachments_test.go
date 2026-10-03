package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"codex-agent/internal/bps"
)

var bpsAttachmentTestPNG = []byte("\x89PNG\r\n\x1a\nsynthetic-private-image-bytes")

func bpsAttachmentTestURI(data []byte) string {
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(data)
}

func bpsAttachmentTestRequest(data []byte) *bps.Request {
	uri, _ := json.Marshal(bpsAttachmentTestURI(data))
	return &bps.Request{Body: []byte(`{"model":"fixture","large":900719925474099312345,"input":[{"type":"message","role":"user","id":"msg_fixture","content":[{"type":"input_text","text":"PRIVATE exact text"},{"type":"input_image","image_url":` + string(uri) + `,"vendor_integer":900719925474099312345}]}]}`)}
}

func bpsAttachmentFixtureAccount(t *testing.T, p *Proxy) (Account, Settings) {
	t.Helper()
	account, err := p.Store.Get("test")
	if err != nil {
		t.Fatal(err)
	}
	settings, err := p.Settings.Get()
	if err != nil {
		t.Fatal(err)
	}
	return account, settings
}

func bpsAttachmentReadFixture(t *testing.T, request *http.Request) []byte {
	t.Helper()
	if request.Method != http.MethodPost || request.URL.Path != "/attachments" {
		t.Errorf("unexpected request method/path: %s %s", request.Method, request.URL.Path)
	}
	if request.Header.Get("Authorization") != "Bearer test-upstream-token" || request.Header.Get("X-Basispoints-Auth-Mode") != "chatgpt" {
		t.Error("missing account authentication")
	}
	reader, err := request.MultipartReader()
	if err != nil {
		t.Error("upload did not use multipart content type")
		return nil
	}
	part, err := reader.NextPart()
	if err != nil {
		t.Error("missing image file part")
		return nil
	}
	if part.FormName() != "file" || part.FileName() != "image.png" || part.Header.Get("Content-Type") != "image/png" {
		t.Error("attachment part differs from official upload contract")
	}
	raw, err := io.ReadAll(part)
	if err != nil {
		t.Error(err)
	}
	if _, err = reader.NextPart(); !errors.Is(err, io.EOF) {
		t.Error("unexpected multipart field")
	}
	return raw
}

func TestBPSAttachmentsUploadOncePreservesBytesAndContext(t *testing.T) {
	var uploads atomic.Int32
	p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if raw := bpsAttachmentReadFixture(t, r); !bytes.Equal(raw, bpsAttachmentTestPNG) {
			t.Error("image bytes changed")
		}
		uploads.Add(1)
		_, _ = io.WriteString(w, `{"openai_file_id":"file_synthetic"}`)
	})
	account, settings := bpsAttachmentFixtureAccount(t, p)
	request := bpsAttachmentTestRequest(bpsAttachmentTestPNG)
	original := bytes.Clone(request.Body)
	for i := 0; i < 2; i++ {
		request.Body = bytes.Clone(original)
		if err := p.prepareBPSAttachments(context.Background(), account, nil, "tenant-a", request, settings); err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(request.Body, []byte("image_url")) || !bytes.Contains(request.Body, []byte(`"file_id":"file_synthetic"`)) || !bytes.Contains(request.Body, []byte(`"detail":"auto"`)) {
			t.Fatal("inline image was not replaced by the returned file reference")
		}
		for _, exact := range []string{"900719925474099312345", "PRIVATE exact text", "msg_fixture"} {
			if !bytes.Contains(request.Body, []byte(exact)) {
				t.Fatal("unrelated history changed")
			}
		}
	}
	if uploads.Load() != 1 {
		t.Fatalf("uploaded %d times for repeated identical owner/image", uploads.Load())
	}
	if !bytes.Contains(original, []byte("image_url")) {
		t.Fatal("original history was mutated")
	}
	if err := p.prepareBPSAttachments(context.Background(), account, nil, "tenant-b", bpsAttachmentTestRequest(bpsAttachmentTestPNG), settings); err != nil {
		t.Fatal(err)
	}
	if uploads.Load() != 2 {
		t.Fatal("tenant cache leaked across keys")
	}
	if err := p.Store.UpdateAccount(account.Name, func(a *Account) error {
		a.AccountID = "new-workspace"
		a.Identity = &managedAccountIdentity{UserID: "new-user", WorkspaceID: a.AccountID}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	changed, err := p.Store.Get(account.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.prepareBPSAttachments(context.Background(), changed, nil, "tenant-a", bpsAttachmentTestRequest(bpsAttachmentTestPNG), settings); err != nil {
		t.Fatal(err)
	}
	if uploads.Load() != 3 {
		t.Fatal("file reference reused across account owner changes")
	}
}

func TestBPSAttachmentsPreserveOtherImageContracts(t *testing.T) {
	var uploads atomic.Int32
	p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		uploads.Add(1)
		t.Error("unexpected upload for opaque/existing/tool image")
		w.WriteHeader(500)
	})
	account, settings := bpsAttachmentFixtureAccount(t, p)
	uri, _ := json.Marshal(bpsAttachmentTestURI(bpsAttachmentTestPNG))
	body := []byte(`{"input":[{"type":"function_call_output","call_id":"call","output":[{"type":"input_image","image_url":` + string(uri) + `}]},{"type":"custom_tool_call_output","call_id":"custom","output":"opaque text containing image_url"},{"type":"message","role":"assistant","content":[{"type":"input_image","image_url":` + string(uri) + `}]},{"type":"message","role":"user","content":[{"type":"input_image","file_id":"file_existing"},{"type":"input_image","image_url":"https://example.invalid/private-image"}]}]}`)
	request := &bps.Request{Body: bytes.Clone(body)}
	if err := p.prepareBPSAttachments(context.Background(), account, nil, "tenant", request, settings); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(request.Body, body) || uploads.Load() != 0 {
		t.Fatal("unrelated image item changed or URL was fetched")
	}
}

func TestBPSAttachmentsCustomToolImagesShareUploadAndPreserveResults(t *testing.T) {
	var uploads atomic.Int32
	p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if !bytes.Equal(bpsAttachmentReadFixture(t, r), bpsAttachmentTestPNG) {
			t.Error("custom output image bytes changed")
		}
		uploads.Add(1)
		_, _ = io.WriteString(w, `{"openai_file_id":"file_custom"}`)
	})
	account, settings := bpsAttachmentFixtureAccount(t, p)
	uri, _ := json.Marshal(bpsAttachmentTestURI(bpsAttachmentTestPNG))
	image := `{"type":"input_image","image_url":` + string(uri) + `}`
	body := []byte(`{"input":[{"type":"custom_tool_call","id":"ctc_a","call_id":"call_a","name":"view","input":"original args"},{"type":"custom_tool_call_output","call_id":"call_a","output":[{"type":"input_text","text":"original result"},` + image + `]},{"type":"custom_tool_call_output","call_id":"call_b","output":[` + image + `]},{"type":"message","role":"user","content":[` + image + `]}],"large":900719925474099312345}`)
	original := bytes.Clone(body)
	request := &bps.Request{Body: body}
	if err := p.prepareBPSAttachments(t.Context(), account, nil, "tenant", request, settings); err != nil {
		t.Fatal(err)
	}
	if uploads.Load() != 1 || bytes.Count(request.Body, []byte(`"file_id":"file_custom"`)) != 3 || bytes.Count(request.Body, []byte(`"detail":"auto"`)) != 3 || bytes.Contains(request.Body, []byte("image_url")) {
		t.Fatal("custom output and message images did not share one upload")
	}
	for _, value := range []string{`"call_id":"call_a"`, `"call_id":"call_b"`, `"input":"original args"`, `"text":"original result"`, `"large":900719925474099312345`} {
		if !bytes.Contains(request.Body, []byte(value)) {
			t.Fatal("unrelated custom output context changed")
		}
	}
	if !bytes.Equal(body, original) {
		t.Fatal("original client history changed")
	}
}

func TestBPSAttachmentsSingleFlightAndWaitCancellation(t *testing.T) {
	entered := make(chan struct{})
	finish := make(chan struct{})
	var uploads atomic.Int32
	p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		bpsAttachmentReadFixture(t, r)
		if uploads.Add(1) == 1 {
			close(entered)
		}
		<-finish
		_, _ = io.WriteString(w, `{"openai_file_id":"file_shared"}`)
	})
	account, settings := bpsAttachmentFixtureAccount(t, p)
	leader := make(chan error, 1)
	go func() {
		leader <- p.prepareBPSAttachments(context.Background(), account, nil, "tenant", bpsAttachmentTestRequest(bpsAttachmentTestPNG), settings)
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("upload did not start")
	}
	waitCtx, cancel := context.WithCancel(context.Background())
	waiter := make(chan error, 1)
	go func() {
		waiter <- p.prepareBPSAttachments(waitCtx, account, nil, "tenant", bpsAttachmentTestRequest(bpsAttachmentTestPNG), settings)
	}()
	cancel()
	select {
	case err := <-waiter:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("waiter cancellation lost")
		}
	case <-time.After(time.Second):
		t.Fatal("canceled waiter blocked behind upload")
	}
	var group sync.WaitGroup
	for i := 0; i < 8; i++ {
		group.Go(func() {
			if err := p.prepareBPSAttachments(context.Background(), account, nil, "tenant", bpsAttachmentTestRequest(bpsAttachmentTestPNG), settings); err != nil {
				t.Error(err)
			}
		})
	}
	close(finish)
	if err := <-leader; err != nil {
		t.Fatal(err)
	}
	group.Wait()
	if uploads.Load() != 1 {
		t.Fatal("same image uploaded concurrently more than once")
	}
}

func TestBPSAttachmentsFailureNoRetryOrPartialRewrite(t *testing.T) {
	for _, status := range []int{401, 403, 422, 502} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var requests atomic.Int32
			p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"error":"PRIVATE_ATTACHMENT_ERROR"}`)
			})
			account, settings := bpsAttachmentFixtureAccount(t, p)
			request := bpsAttachmentTestRequest(bpsAttachmentTestPNG)
			original := bytes.Clone(request.Body)
			err := p.prepareBPSAttachments(context.Background(), account, nil, "tenant", request, settings)
			var upload *bpsAttachmentError
			if !errors.As(err, &upload) || upload.StatusCode != status || strings.Contains(err.Error(), "PRIVATE_ATTACHMENT_ERROR") {
				t.Fatalf("unsafe or wrong upload error: %v", err)
			}
			if requests.Load() != 1 || !bytes.Equal(request.Body, original) || len(p.BPS.attachments.entries) != 0 {
				t.Fatal("failed upload was retried, cached or partially rewrote history")
			}
		})
	}
}

func TestBPSAttachmentsOwnerChangeAndMalformedImage(t *testing.T) {
	var p *Proxy
	var requests atomic.Int32
	p = bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		bpsAttachmentReadFixture(t, r)
		if err := p.Store.UpdateAccount("test", func(a *Account) error { a.AccountID = "replacement-owner"; return nil }); err != nil {
			t.Error(err)
		}
		_, _ = io.WriteString(w, `{"openai_file_id":"file_old_owner"}`)
	})
	account, settings := bpsAttachmentFixtureAccount(t, p)
	for _, uri := range []string{"data:image/png;base64,INVALID!", "data:image/svg+xml;base64,PHN2Zy8+", "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("not png"))} {
		encoded, _ := json.Marshal(uri)
		request := &bps.Request{Body: []byte(`{"input":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":` + string(encoded) + `}]}]}`)}
		err := p.prepareBPSAttachments(context.Background(), account, nil, "tenant", request, settings)
		var invalid *bpsAttachmentError
		if !errors.As(err, &invalid) || invalid.StatusCode != 400 {
			t.Fatal("malformed image was not rejected locally")
		}
	}
	if requests.Load() != 0 {
		t.Fatal("malformed image dispatched upstream")
	}
	request := bpsAttachmentTestRequest(bpsAttachmentTestPNG)
	original := bytes.Clone(request.Body)
	err := p.prepareBPSAttachments(context.Background(), account, nil, "tenant", request, settings)
	var owner *gatewayOwnerChangedError
	if !errors.As(err, &owner) || !bytes.Equal(request.Body, original) || len(p.BPS.attachments.entries) != 0 {
		t.Fatal("new owner received old upload reference")
	}
}

func TestBPSAttachmentsCacheExpiryAndScope(t *testing.T) {
	cache := bpsAttachmentCache{entries: map[string]bpsAttachmentEntry{"image": {fileID: "expired", expires: time.Now().Add(-time.Second)}}}
	id, err := cache.file(context.Background(), "image", func() (string, error) { return "fresh", nil })
	if err != nil || id != "fresh" {
		t.Fatal("expired reference was reused")
	}
	account := Account{ID: "account", AccountID: "workspace", Identity: &managedAccountIdentity{UserID: "user"}}
	key := bpsAttachmentCacheKey("tenant", account, nil, "https://example.invalid/api", "image/png", bpsAttachmentTestPNG)
	for _, other := range []string{
		bpsAttachmentCacheKey("other", account, nil, "https://example.invalid/api", "image/png", bpsAttachmentTestPNG),
		bpsAttachmentCacheKey("tenant", account, nil, "https://different.invalid/api", "image/png", bpsAttachmentTestPNG),
		bpsAttachmentCacheKey("tenant", account, &ProxyEntry{ID: "proxy"}, "https://example.invalid/api", "image/png", bpsAttachmentTestPNG),
		bpsAttachmentCacheKey("tenant", account, nil, "https://example.invalid/api", "image/jpeg", bpsAttachmentTestPNG),
	} {
		if key == other {
			t.Fatal("cache scope was not isolated")
		}
	}
}

func TestBPSAttachmentsAuthorizationOnHitAndNoImageFastPath(t *testing.T) {
	var requests atomic.Int32
	p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		bpsAttachmentReadFixture(t, r)
		_, _ = io.WriteString(w, `{"openai_file_id":"file_authorized"}`)
	})
	account, settings := bpsAttachmentFixtureAccount(t, p)
	denied := errors.New("synthetic key revoked")
	authCalls := 0
	check := func(context.Context) error { authCalls++; return denied }
	textOnly := &bps.Request{Body: []byte(`{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"OK"}]}]}`)}
	if err := p.prepareBPSAttachments(context.Background(), account, nil, "tenant", textOnly, settings, check); err != nil || authCalls != 0 {
		t.Fatal("text-only request performed unnecessary authorization")
	}
	if err := p.prepareBPSAttachments(context.Background(), account, nil, "tenant", bpsAttachmentTestRequest(bpsAttachmentTestPNG), settings); err != nil {
		t.Fatal(err)
	}
	request := bpsAttachmentTestRequest(bpsAttachmentTestPNG)
	original := bytes.Clone(request.Body)
	if err := p.prepareBPSAttachments(context.Background(), account, nil, "tenant", request, settings, check); !errors.Is(err, denied) {
		t.Fatal("cache hit bypassed revoked authorization")
	}
	if requests.Load() != 1 || !bytes.Equal(original, request.Body) {
		t.Fatal("revoked key uploaded or received a cached file reference")
	}
}

func TestBPSAttachmentsUploadCancellationAndInvalidReply(t *testing.T) {
	t.Run("canceled upload", func(t *testing.T) {
		entered, exited := make(chan struct{}), make(chan struct{})
		p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
			bpsAttachmentReadFixture(t, r)
			close(entered)
			<-r.Context().Done()
			close(exited)
		})
		account, settings := bpsAttachmentFixtureAccount(t, p)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		request := bpsAttachmentTestRequest(bpsAttachmentTestPNG)
		original := bytes.Clone(request.Body)
		done := make(chan error, 1)
		go func() { done <- p.prepareBPSAttachments(ctx, account, nil, "tenant", request, settings) }()
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("upload did not start")
		}
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatal("upload cancellation lost")
			}
		case <-time.After(time.Second):
			t.Fatal("upload ignored cancellation")
		}
		select {
		case <-exited:
		case <-time.After(time.Second):
			t.Fatal("upstream upload remained active")
		}
		if !bytes.Equal(original, request.Body) || len(p.BPS.attachments.entries) != 0 {
			t.Fatal("canceled upload changed history or cache")
		}
	})
	for _, reply := range []string{`{}`, `{"openai_file_id":" "}`, `{"openai_file_id":false}`, `{"openai_file_id":"file_partial"} {}`} {
		t.Run(reply, func(t *testing.T) {
			p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				bpsAttachmentReadFixture(t, r)
				_, _ = io.WriteString(w, reply)
			})
			account, settings := bpsAttachmentFixtureAccount(t, p)
			request := bpsAttachmentTestRequest(bpsAttachmentTestPNG)
			original := bytes.Clone(request.Body)
			err := p.prepareBPSAttachments(context.Background(), account, nil, "tenant", request, settings)
			var invalid *bpsAttachmentError
			if !errors.As(err, &invalid) || invalid.StatusCode != 502 || !bytes.Equal(original, request.Body) || len(p.BPS.attachments.entries) != 0 {
				t.Fatal("invalid upload response was accepted or cached")
			}
		})
	}
}

func TestBPSAttachmentsMultipleImagesFailAtomicallyAndEscapedFieldWorks(t *testing.T) {
	var requests atomic.Int32
	p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		bpsAttachmentReadFixture(t, r)
		if requests.Add(1) == 2 {
			w.WriteHeader(502)
			return
		}
		_, _ = io.WriteString(w, `{"openai_file_id":"file_first"}`)
	})
	account, settings := bpsAttachmentFixtureAccount(t, p)
	uri1, _ := json.Marshal(bpsAttachmentTestURI(bpsAttachmentTestPNG))
	uri2, _ := json.Marshal(bpsAttachmentTestURI(append(bytes.Clone(bpsAttachmentTestPNG), '2')))
	request := &bps.Request{Body: []byte(`{"input":[{"type":"message","role":"user","content":[{"type":"input_image","image\u005furl":` + string(uri1) + `},{"type":"input_image","image\u005furl":` + string(uri2) + `}]}]}`)}
	original := bytes.Clone(request.Body)
	err := p.prepareBPSAttachments(context.Background(), account, nil, "tenant", request, settings)
	var failed *bpsAttachmentError
	if !errors.As(err, &failed) || requests.Load() != 2 || !bytes.Equal(original, request.Body) {
		t.Fatal("multi-image error partially changed outbound body or skipped escaped keys")
	}
	if len(p.BPS.attachments.entries) != 1 {
		t.Fatal("failed upload cached or successful first image lost")
	}
}

func TestBPSAttachmentsImplicitMessageRetainsOriginalShape(t *testing.T) {
	var requests atomic.Int32
	p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		bpsAttachmentReadFixture(t, r)
		_, _ = io.WriteString(w, `{"openai_file_id":"file_implicit"}`)
	})
	account, settings := bpsAttachmentFixtureAccount(t, p)
	uri, _ := json.Marshal(bpsAttachmentTestURI(bpsAttachmentTestPNG))
	for _, role := range []string{"user", "developer", "system"} {
		request := &bps.Request{Body: []byte(`{"input":[{"role":"` + role + `","content":[{"type":"input_image","image_\u0075rl":` + string(uri) + `,"detail":"high"}]}]}`)}
		if err := p.prepareBPSAttachments(context.Background(), account, nil, "tenant", request, settings); err != nil {
			t.Fatal(err)
		}
		var decoded struct {
			Input []map[string]json.RawMessage `json:"input"`
		}
		if json.Unmarshal(request.Body, &decoded) != nil || len(decoded.Input) != 1 {
			t.Fatal("rewritten implicit message invalid")
		}
		if _, exists := decoded.Input[0]["type"]; exists {
			t.Fatal("implicit message type was injected")
		}
		if !bytes.Contains(request.Body, []byte(`"file_id":"file_implicit"`)) || !bytes.Contains(request.Body, []byte(`"detail":"high"`)) {
			t.Fatal("implicit escaped image or explicit detail was lost")
		}
	}
	if requests.Load() != 1 {
		t.Fatal("same image reuploaded across message roles")
	}
}
