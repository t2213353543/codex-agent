package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// Synthetic, offline benchmarks for the actual BPS WebSocket SSE adapter and
// continuation boundary. No provider, account credentials or network is used.
func BenchmarkBPSWSAdapter(b *testing.B) {
	for _, size := range []int{32, 1024, 65536} {
		for _, lane := range []string{"", "lane_1"} {
			b.Run(fmt.Sprintf("delta_%d/lane_%t", size, lane != ""), func(b *testing.B) {
				raw := []byte(`event: response.output_text.delta` + "\ndata: " + `{"type":"response.output_text.delta","delta":"` + strings.Repeat("x", size) + `"}` + "\n\n")
				w := &bpsWSResponseWriter{streamID: lane, send: func(raw []byte) error { return nil }}
				b.ReportAllocs()
				b.SetBytes(int64(len(raw)))
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, err := w.Write(raw); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

func BenchmarkBPSHistory(b *testing.B) {
	for _, count := range []int{1, 32, 256} {
		input := make([]json.RawMessage, count)
		for i := range input {
			input[i] = json.RawMessage(`{"role":"user","content":"` + strings.Repeat("x", 1024) + `"}`)
		}
		encoded, _ := json.Marshal(input)
		request := []byte(`{"model":"synthetic-model","stream":true,"input":` + string(encoded) + `}`)
		record := bpsHistoryRecord{Owner: "owner", Session: "session", Request: request, Output: []json.RawMessage{json.RawMessage(`{"role":"assistant","content":"previous"}`)}}
		p := &Proxy{BPS: &bpsRuntime{}}
		ctx := context.Background()
		if err := p.bpsSaveHistory(ctx, "key", "resp_previous", record); err != nil {
			b.Fatal(err)
		}
		b.Run(fmt.Sprintf("fresh_%d", count), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(request)))
			for i := 0; i < b.N; i++ {
				if _, _, _, err := p.bpsExpandHistory(ctx, "key", request, ""); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(fmt.Sprintf("expand_%d", count), func(b *testing.B) {
			increment := []byte(`{"previous_response_id":"resp_previous","input":[{"role":"user","content":"next"}]}`)
			b.ReportAllocs()
			b.SetBytes(int64(len(request)))
			for i := 0; i < b.N; i++ {
				if _, _, _, err := p.bpsExpandHistory(ctx, "key", increment, ""); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(fmt.Sprintf("load_%d", count), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(request)))
			for i := 0; i < b.N; i++ {
				if _, err := p.bpsLoadHistory(ctx, "key", "resp_previous"); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
