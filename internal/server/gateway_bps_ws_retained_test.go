package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// Reflection permits the same experiment against the original writer overlay,
// which has no eventBuffer. Capacity is a conservative visible reuse metric;
// the original writer's sliced tail can still reference a larger backing array.
func bpsWSRetainedCapacity(w *bpsWSResponseWriter) int {
	capacity := cap(w.buffer)
	if event := reflect.ValueOf(w).Elem().FieldByName("eventBuffer"); event.IsValid() {
		capacity += event.Cap()
	}
	return capacity
}

func bpsWSRetainedFrame(size int) []byte {
	return []byte(`data: {"type":"response.output_text.delta","delta":"` + strings.Repeat("x", size) + `"}` + "\n\n")
}

func TestBPSWSAdapterRetainedCapacity(t *testing.T) {
	for _, lane := range []string{"", "lane_1"} {
		t.Run(fmt.Sprintf("lane_%t", lane != ""), func(t *testing.T) {
			large, small := bpsWSRetainedFrame(1<<20), bpsWSRetainedFrame(32)
			writes := 0
			w := &bpsWSResponseWriter{streamID: lane, send: func(raw []byte) error {
				var frame struct {
					Delta string `json:"delta"`
				}
				if json.Unmarshal(raw, &frame) != nil {
					t.Fatal("invalid frame")
				}
				want := 32
				if writes == 0 {
					want = 1 << 20
				}
				if len(frame.Delta) != want {
					t.Fatalf("truncated frame: %d != %d", len(frame.Delta), want)
				}
				writes++
				return nil
			}}
			if _, err := w.Write(large); err != nil {
				t.Fatal(err)
			}
			t.Logf("after_1MiB: retained_capacity_bytes=%d", bpsWSRetainedCapacity(w))
			if bpsWSRetainedCapacity(w) > 2*(256<<10) {
				t.Fatal("completed large frame retained oversized reuse buffers")
			}
			for i := 0; i < 128; i++ {
				if _, err := w.Write(small); err != nil {
					t.Fatal(err)
				}
				if i == 0 {
					t.Logf("after_1MiB_then_1_small: retained_capacity_bytes=%d", bpsWSRetainedCapacity(w))
				}
			}
			t.Logf("after_1MiB_then_128_small: retained_capacity_bytes=%d", bpsWSRetainedCapacity(w))
			if writes != 129 {
				t.Fatalf("frame count: %d", writes)
			}
		})
	}
}

func TestBPSWSAdapterLargeFragmentLifetime(t *testing.T) {
	large, next := bpsWSRetainedFrame(1<<20), bpsWSRetainedFrame(32)
	var got [][]byte
	w := &bpsWSResponseWriter{streamID: "lane_1", send: func(raw []byte) error { got = append(got, bytes.Clone(raw)); return nil }}
	// An incomplete large frame must stay intact even above the reuse limit.
	if _, err := w.Write(large[:600<<10]); err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 || !bytes.Equal(w.buffer, large[:600<<10]) {
		t.Fatal("incomplete large frame truncated or emitted early")
	}
	// Complete the large event alongside a pending next-event prefix.
	first := append(bytes.Clone(large[600<<10:]), next[:19]...)
	if _, err := w.Write(first); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !bytes.Equal(w.buffer, next[:19]) {
		t.Fatal("pending fragment lost or emitted early")
	}
	if bpsWSRetainedCapacity(w) > 2*(256<<10) {
		t.Fatal("small pending fragment retained a completed large frame allocation")
	}
	if _, err := w.Write(next[19:]); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !bytes.Contains(got[0], bytes.Repeat([]byte("x"), 1<<20)) || !bytes.Contains(got[1], bytes.Repeat([]byte("x"), 32)) {
		t.Fatal("completed frame changed after next write")
	}
	if len(w.buffer) != 0 {
		t.Fatal("fragment remained after complete send")
	}
}

func BenchmarkBPSWSAdapterLargeThenSmall(b *testing.B) {
	large, small := bpsWSRetainedFrame(1<<20), bpsWSRetainedFrame(32)
	w := &bpsWSResponseWriter{streamID: "lane_1", send: func([]byte) error { return nil }}
	b.SetBytes(int64(len(large) + 128*len(small)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := w.Write(large); err != nil {
			b.Fatal(err)
		}
		for i := 0; i < 128; i++ {
			if _, err := w.Write(small); err != nil {
				b.Fatal(err)
			}
		}
	}
}
