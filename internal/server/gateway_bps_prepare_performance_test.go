package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"codex-agent/internal/bps"
)

func TestBPSPreparedAccountBindingMatchesFullPreparation(t *testing.T) {
	account := Account{AccountID: "fixture-workspace", AccessToken: "fixture-token"}
	settings := Settings{PromptCacheKeyEnabled: true}
	for _, controls := range []string{
		``,
		`,"prompt_cache_key":"cache"`,
		`,"prompt_cache_key":null`,
		`,"prompt_cache_key":""`,
		`,"prompt_cache_key":"session","client_metadata":{"session_id":"session","thread_id":"thread","opaque":9007199254740993}`,
		`,"prompt_cache_key":"first","prompt_cache_key":"last","client_metadata":{"session_id":"session","x-codex-turn-metadata":"{\"thread_id\":\"t\"}"}`,
		`,"prompt_\u0063ache_key":"cache","client_metadata":null`,
	} {
		for _, compact := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/compact=%t", controls, compact), func(t *testing.T) {
				input := []byte(`{"model":"fixture","stream":true,"input":[{"role":"user","content":"<literal> 世界"},{"type":"function_call_output","call_id":"call_1","output":"9007199254740993"}],"metadata":{"attachment_file_ids":["a"],"agent_iteration":"2"}` + controls + `}`)
				original := append([]byte(nil), input...)
				opt := bps.Options{TaskID: "before-account", TurnID: "same-turn", Compact: compact}
				actual, err := bps.Prepare(input, opt)
				if err != nil {
					t.Fatal(err)
				}
				scoped, err := prepareGatewayAccountPromptCache(input, "key", account, settings)
				if err != nil {
					t.Fatal(err)
				}
				opt.TaskID = "after-account"
				want, err := bps.Prepare(scoped, opt)
				if err != nil {
					t.Fatal(err)
				}
				if err := bpsBindPreparedAccount(actual, scoped, opt.TaskID); err != nil {
					t.Fatal(err)
				}
				var a, b map[string]json.RawMessage
				if json.Unmarshal(actual.Body, &a) != nil || json.Unmarshal(want.Body, &b) != nil || !reflect.DeepEqual(a, b) {
					t.Fatalf("binding differs from full preparation\nactual: %s\nwant: %s", actual.Body, want.Body)
				}
				if !reflect.DeepEqual(actual.Policy.ClientControls, want.Policy.ClientControls) || actual.TaskID != want.TaskID || actual.TurnID != want.TurnID {
					t.Fatal("local policy or request identity differs")
				}
				if !bytes.Equal(input, original) {
					t.Fatal("mutated history bytes")
				}
			})
		}
	}
}

func TestBPSPreparedAccountBindingKeepsToolsAndTurn(t *testing.T) {
	input := []byte(`{"model":"fixture","input":[],"tools":[{"type":"function","name":"inspect","parameters":{"type":"object","properties":{"integer":{"const":9007199254740993}}}}],"tool_choice":"required","parallel_tool_calls":false,"prompt_cache_key":"cache"}`)
	prepared, err := bps.Prepare(input, bps.Options{TaskID: "before", TurnID: "one-turn"})
	if err != nil {
		t.Fatal(err)
	}
	var before, after map[string]json.RawMessage
	_ = json.Unmarshal(prepared.Body, &before)
	catalog := prepared.Tools
	scoped, err := prepareGatewayAccountPromptCache(input, "key", Account{AccountID: "fixture", AccessToken: "token"}, Settings{PromptCacheKeyEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := bpsBindPreparedAccount(prepared, scoped, "after"); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(prepared.Body, &after)
	if !bytes.Equal(before["input"], after["input"]) || prepared.TurnID != "one-turn" || catalog != prepared.Tools {
		t.Fatal("account binding regenerated tools, input, catalog or turn identity")
	}
	if err := prepared.Tools.ValidateResponse(nil, prepared.Policy); err == nil {
		t.Fatal("tool_choice required was lost after binding")
	}
	if prepared.Policy.ParallelToolCalls == nil || *prepared.Policy.ParallelToolCalls {
		t.Fatal("parallel tool policy was lost after binding")
	}
}

func BenchmarkBPSAccountPreparation(b *testing.B) {
	for _, size := range []int{32, 256 << 10, 1 << 20} {
		input, _ := json.Marshal(map[string]any{"model": "fixture", "stream": true, "input": []any{map[string]string{"role": "user", "content": strings.Repeat("x", size)}}, "prompt_cache_key": "session", "client_metadata": map[string]string{"session_id": "session"}})
		for _, reuse := range []bool{false, true} {
			b.Run(fmt.Sprintf("bytes=%d/reuse=%t", size, reuse), func(b *testing.B) {
				account := Account{AccountID: "fixture", AccessToken: "fixture-token"}
				opt := bps.Options{TaskID: "before-account", TurnID: "same-turn"}
				b.ReportAllocs()
				b.SetBytes(int64(len(input)))
				for b.Loop() {
					prepared, err := bps.Prepare(input, opt)
					if err != nil {
						b.Fatal(err)
					}
					scoped, err := prepareGatewayAccountPromptCache(input, "key", account, Settings{PromptCacheKeyEnabled: true})
					if err != nil {
						b.Fatal(err)
					}
					if reuse {
						err = bpsBindPreparedAccount(prepared, scoped, "after-account")
					} else {
						final := opt
						final.TaskID = "after-account"
						prepared, err = bps.Prepare(scoped, final)
					}
					if err != nil || prepared.TaskID != "after-account" {
						b.Fatalf("prepare: %v", err)
					}
				}
			})
		}
	}
}
