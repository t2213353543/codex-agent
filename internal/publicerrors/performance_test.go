package publicerrors

import (
	"encoding/json"
	"fmt"
	"testing"
)

func BenchmarkJSONSanitization(b *testing.B) {
	for _, failures := range []bool{false, true} {
		b.Run(fmt.Sprintf("management_500_errors_%t", failures), func(b *testing.B) {
			items := make([]map[string]any, 500)
			for i := range items {
				item := map[string]any{"id": i, "mode": "powerpoint", "model": "synthetic", "tokens": 9007199254740993,
					"observation": map[string]any{"path": "/responses", "count": 14, "latency": 25, "enabled": true},
					"groups":      []string{"group-one", "group-two"}, "error": ""}
				if failures && i%10 == 0 {
					item["error"] = "PowerPoint upstream failed"
				}
				items[i] = item
			}
			raw, _ := json.Marshal(map[string]any{"items": items, "total": 500})
			b.ReportAllocs()
			b.SetBytes(int64(len(raw)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if len(JSON(raw, false)) == 0 {
					b.Fatal("lost response")
				}
			}
		})
	}
}
