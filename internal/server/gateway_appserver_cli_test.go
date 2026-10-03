package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const appServerCLITestModel = `{"slug":"fixture","use_responses_lite":true,"default_reasoning_level":"medium","default_reasoning_summary":"none","support_verbosity":true,"default_verbosity":"low","model_messages":{"instructions_template":"CATALOG INSTRUCTIONS"}}`

func TestAppServerCLIClientContract(t *testing.T) {
	raw := []byte(`{"model":"fixture","input":"hello","tools":[{"type":"function","name":"inspect","parameters":{"type":"object","properties":{"integer":{"const":9007199254740993}}}},{"type":"custom","name":"patch","format":{"type":"grammar","syntax":"lark","definition":"start: /.+/"}}],"tool_choice":{"type":"function","name":"inspect"},"reasoning":{"effort":"xhigh","encrypted_extension":{"v":9007199254740993}},"prompt_cache_key":"caller-cache","text":{"format":{"type":"json_schema","name":"client_schema","schema":{"type":"object"},"strict":true}},"future_extension":{"large":9007199254740993}}`)
	h := http.Header{}
	h.Set("Session-Id", "session")
	h.Set("Thread-Id", "thread")
	h.Set("Originator", "caller")
	in, _, err := prepareAppServerCLI(raw, h, json.RawMessage(appServerCLITestModel), "install")
	if err != nil {
		t.Fatal(err)
	}
	wire, err := buildAppServerRequest(in)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]json.RawMessage
	if err = json.Unmarshal(wire.Body, &out); err != nil {
		t.Fatal(err)
	}
	var input []map[string]json.RawMessage
	_ = json.Unmarshal(out["input"], &input)
	if len(input) != 3 || string(input[0]["type"]) != `"additional_tools"` || !strings.Contains(string(input[1]["content"]), "CATALOG INSTRUCTIONS") {
		t.Fatalf("unexpected writer prefix %s", out["input"])
	}
	if strings.Contains(string(out["input"]), "clock") || strings.Contains(string(out["input"]), "multi_agent_role") {
		t.Fatal("added undeclared runtime capabilities")
	}
	for _, field := range []string{"reasoning", "future_extension", "text", "tool_choice"} {
		if len(out[field]) == 0 {
			t.Fatalf("missing %s", field)
		}
	}
	if !strings.Contains(string(out["reasoning"]), `"context":"all_turns"`) || !strings.Contains(string(out["reasoning"]), `"effort":"xhigh"`) || !strings.Contains(string(out["reasoning"]), "9007199254740993") {
		t.Fatalf("reasoning changed %s", out["reasoning"])
	}
	if string(out["prompt_cache_key"]) != `"caller-cache"` || strings.Contains(string(out["tool_choice"]), `"namespace"`) || !strings.Contains(string(out["tool_choice"]), `"name":"inspect"`) || !strings.Contains(string(out["text"]), "client_schema") {
		t.Fatal("client control lost")
	}
	if wire.Headers.Get("Thread-Id") != "thread" || wire.Headers.Get("Session-Id") != "session" {
		t.Fatal("client identities changed")
	}
}

func TestAppServerCLICatalogCredentialIsolation(t *testing.T) {
	var mu sync.Mutex
	calls := map[string]int{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/codex/models" || r.Header.Get("Content-Encoding") != "" {
			t.Error("invalid model fetch")
		}
		identity := r.Header.Get("Authorization")
		mu.Lock()
		calls[identity]++
		mu.Unlock()
		if identity == "Bearer rejected" {
			w.WriteHeader(401)
			_, _ = io.WriteString(w, `{"error":{"code":"expired"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"models":[`+appServerCLITestModel+`]}`)
	}))
	defer up.Close()
	a := &appServerCLI{catalog: map[[32]byte]appServerCatalogSnapshot{}}
	for _, identity := range []string{"first", "first", "second", "rejected", "rejected"} {
		r, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, up.URL+"/backend-api/codex/responses", nil)
		r.Header.Set("Authorization", "Bearer "+identity)
		r.Header.Set("Content-Encoding", "zstd")
		m, res, err := a.model(r, up.Client(), nil, "fixture")
		if err != nil {
			t.Fatal(err)
		}
		if identity == "rejected" {
			if res == nil || res.StatusCode != 401 {
				t.Fatal("lost auth rejection")
			}
			b, _ := io.ReadAll(res.Body)
			_ = res.Body.Close()
			if !strings.Contains(string(b), "expired") {
				t.Fatal("lost auth body")
			}
		} else if m == nil || res != nil {
			t.Fatal("missing model")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if calls["Bearer first"] != 1 || calls["Bearer second"] != 1 || calls["Bearer rejected"] != 2 {
		t.Fatalf("catalog scopes/rejection cached: %v", calls)
	}
}

func TestAppServerCLIMappedStreamAssemblesClientOutput(t *testing.T) {
	adapter, err := buildAppServerTools(json.RawMessage(`[{"type":"function","name":"inspect","parameters":{"type":"object"}}]`), true)
	if err != nil {
		t.Fatal(err)
	}
	sse := `data: {"type":"response.created","response":{"id":"resp_one","status":"in_progress","output":[]}}` + "\n\n" + `data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_one","call_id":"call_one","namespace":"functions","name":"inspect","arguments":"{}"}}` + "\n\n" + `data: {"type":"response.completed","response":{"id":"resp_one","status":"completed","model":"fixture","output":[],"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}}}` + "\n\n"
	stream := newAppServerMappedStream(io.NopCloser(strings.NewReader(sse)), adapter)
	defer stream.Close()
	result, err := collectResponse(stream)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Output []struct{ Name, Namespace, CallID string }
		Usage  json.RawMessage
	}
	if json.Unmarshal(result, &body) != nil || len(body.Output) != 1 || body.Output[0].Name != "inspect" || body.Output[0].Namespace != "" || !strings.Contains(string(body.Usage), `"total_tokens":10`) {
		t.Fatalf("tool/usage contract lost: %s", result)
	}
}
