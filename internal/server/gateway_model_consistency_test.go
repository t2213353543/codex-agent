package server

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestModelConsistencyHTTPAndBPSResponseOwnership(t *testing.T) {
	for _, bpsMode := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			for _, responseModel := range []string{"gpt-6-astra", "different-model", ""} {
				name := "http"
				if bpsMode {
					name = "bps"
				}
				if stream {
					name += "_stream"
				}
				t.Run(name+"/"+responseModel, func(t *testing.T) {
					var wireModel string
					handler := func(w http.ResponseWriter, r *http.Request) {
						var request struct {
							Model string `json:"model"`
						}
						if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
							t.Error(err)
						}
						wireModel = request.Model
						w.Header().Set("Content-Type", "text/event-stream")
						w.Header().Set("Openai-Model", "gpt-6-astra")
						created, _ := json.Marshal(map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_actual", "model": responseModel}})
						_, _ = io.WriteString(w, "data: "+string(created)+"\n\n")
						_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_actual\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":4,\"output_tokens\":1}}}\n\n")
					}
					var p *Proxy
					if bpsMode {
						p = bpsGatewayFixture(t, handler)
					} else {
						p = proxyFixture(t, handler)
					}
					payload, _ := json.Marshal(map[string]any{"model": "gpt-6-astra", "input": []any{}, "stream": stream})
					w := httptest.NewRecorder()
					p.ServeHTTP(w, proxyRequest(http.MethodPost, "/v1/responses", string(payload)))
					if w.Code != http.StatusOK {
						t.Fatalf("HTTP %d: %s", w.Code, w.Body.String())
					}
					rec, ok := p.Metrics.LastFor("test")
					if !ok || rec.Status != "ok" || rec.SentModel != wireModel || rec.ReturnedModel != responseModel {
						t.Fatalf("wrong wire observations: sent=%q wire=%q returned=%q expected=%q status=%q", rec.SentModel, wireModel, rec.ReturnedModel, responseModel, rec.Status)
					}
					want := "mismatch"
					if responseModel == "" {
						want = "unknown"
					} else if responseModel == wireModel {
						want = "match"
					}
					if got := managedRecord(rec).ModelConsistency; got != want {
						t.Fatalf("consistency=%q want=%q", got, want)
					}
				})
			}
		}
	}
}

func TestModelConsistencyForeignSSECannotOverwriteReturnedModel(t *testing.T) {
	p := proxyFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_actual\",\"model\":\"actual-model\"}}\n\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.in_progress\",\"response\":{\"id\":\"resp_foreign\",\"model\":\"foreign-model\"}}\n\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_actual\",\"status\":\"completed\",\"output\":[]}}\n\n")
	})
	w := httptest.NewRecorder()
	p.ServeHTTP(w, proxyRequest(http.MethodPost, "/v1/responses", `{"model":"actual-model","input":[],"stream":true}`))
	record, ok := p.Metrics.LastFor("test")
	if !ok || record.ReturnedModel != "actual-model" || gatewayModelConsistency(record) != "match" {
		t.Fatalf("unowned event overwrote model: sent=%q returned=%q", record.SentModel, record.ReturnedModel)
	}
}

func TestModelConsistencyStoredLogsAndExports(t *testing.T) {
	m, err := NewMetrics(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	for index, pair := range [][2]string{{"actual", "actual"}, {"actual", "other"}, {"actual", ""}, {"", "actual"}} {
		m.Record(RequestRecord{Time: 1700000000 + int64(index), Model: "alias", SentModel: pair[0], ReturnedModel: pair[1], UpstreamModel: "actual", Status: "ok"})
	}
	q, err := parseManagedLogQuery(url.Values{})
	if err != nil {
		t.Fatal(err)
	}
	page, err := m.managedLogs(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	wants := []string{"unknown", "unknown", "mismatch", "match"}
	if len(page.Items) != len(wants) {
		t.Fatalf("records=%d", len(page.Items))
	}
	for index, record := range page.Items {
		if record.ModelConsistency != wants[index] {
			t.Fatalf("stored consistency=%q want=%q", record.ModelConsistency, wants[index])
		}
	}
	for _, format := range []string{"jsonl", "csv"} {
		var output bytes.Buffer
		if _, err = m.exportManagedLogsWithPrivacy(context.Background(), q, format, &output, true); err != nil {
			t.Fatal(err)
		}
		if format == "jsonl" {
			decoder := json.NewDecoder(&output)
			for _, want := range wants {
				var record managedLogRecord
				if err = decoder.Decode(&record); err != nil {
					t.Fatal(err)
				}
				if record.ModelConsistency != want {
					t.Fatalf("JSONL consistency=%q want=%q", record.ModelConsistency, want)
				}
			}
		} else {
			rows, err := csv.NewReader(strings.NewReader(output.String())).ReadAll()
			if err != nil {
				t.Fatal(err)
			}
			column := -1
			for index, value := range rows[0] {
				if value == "model_consistency" {
					column = index
				}
			}
			if column < 0 {
				t.Fatal("CSV omitted model consistency")
			}
			for index, want := range wants {
				if rows[index+1][column] != want {
					t.Fatalf("CSV consistency=%q want=%q", rows[index+1][column], want)
				}
			}
		}
	}
}

func TestModelConsistencyTruncatedOrRedactedIsUnknown(t *testing.T) {
	for _, model := range []string{strings.Repeat("m", 512), strings.Repeat("模型", 85), strings.Repeat("m", 509), "[REDACTED]"} {
		if got := gatewayModelConsistency(RequestRecord{SentModel: model, ReturnedModel: model}); got != "unknown" {
			t.Fatalf("incomplete model treated as %q", got)
		}
	}
}
