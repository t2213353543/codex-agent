package server

import (
	"context"
	"fmt"
	"testing"
)

func BenchmarkSystemPGKeyCurrentSnapshot(b *testing.B) {
	for _, count := range []int{100, 10000} {
		b.Run(fmt.Sprintf("keys_%d", count), func(b *testing.B) {
			p := systemResourceBenchmarkProxy(b, 1000, count)
			// Use an enabled key at the end of the document to capture the former
			// decrypt/decode/linear-lookup cost and verify identical lookup output.
			if err := p.Keys.store.Modify(func(state *keyState) error {
				state.Keys[len(state.Keys)-1].Enabled = true
				return nil
			}); err != nil {
				b.Fatal(err)
			}
			token := fmt.Sprintf("synthetic-never-use-%d", count)
			ctx := context.Background()
			if _, found, err := p.Keys.LookupCurrent(ctx, token); err != nil || !found {
				b.Fatalf("warm lookup: %t %v", found, err)
			}
			b.Run("decrypt_each_read", func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					var state keyState
					if err := p.Keys.store.db.readDocument(ctx, p.Keys.store.db.Pool, p.Keys.store.key, &state); err != nil {
						b.Fatal(err)
					}
					found := false
					for _, key := range state.Keys {
						if key.Key == token && key.Enabled {
							found = true
						}
					}
					if !found {
						b.Fatal("legacy lookup did not find the current key")
					}
				}
			})
			b.Run("authoritative_snapshot", func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					if _, found, err := p.Keys.LookupCurrent(ctx, token); err != nil || !found {
						b.Fatalf("current lookup: %t %v", found, err)
					}
				}
			})
		})
	}
}
