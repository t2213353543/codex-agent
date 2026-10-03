package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"testing/synctest"
	"time"

	"github.com/klauspost/compress/zstd"
)

type performanceAuditChunkReader struct {
	io.Reader
	chunk int
}

func (r performanceAuditChunkReader) Read(p []byte) (int, error) {
	return r.Reader.Read(p[:min(len(p), r.chunk)])
}

func TestGatewayPerformanceAuditMaximumFragmentedSSELine(t *testing.T) {
	// CR is the last byte of an upstream read; the following LF must not turn
	// into a second line or restart a scan over the accumulated 32 MiB prefix.
	line := ":" + strings.Repeat("x", maxBody-3) + "\r\n"
	terminal := "data: " + auditSSETerminal + "\n\n"
	reader := performanceAuditChunkReader{Reader: io.MultiReader(strings.NewReader(line[:len(line)-1]), strings.NewReader(line[len(line)-1:]), strings.NewReader(terminal)), chunk: 64 << 10}
	got, err := collectResponse(reader)
	if err != nil || !bytes.Contains(got, []byte(`"id":"resp_audit"`)) {
		t.Fatalf("maximum fragmented comment lost subsequent terminal: %v", err)
	}
	reader = performanceAuditChunkReader{Reader: io.MultiReader(strings.NewReader("x"+line), strings.NewReader(terminal)), chunk: 64 << 10}
	if result, err := collectResponse(reader); err == nil || result != nil {
		t.Fatal("fragmented line bypassed the 32 MiB budget")
	}
}

func TestGatewayPerformanceAuditManyItemsAndSparseIndices(t *testing.T) {
	var a responseOutputAccumulator
	for i := range maxCollectedResponseItems {
		index := int64(i) * (1 << 40)
		event := fmt.Appendf(nil, `{"type":"response.output_item.done","output_index":%d,"item":{"id":"i%d","future":9007199254740993,"opaque":"你好🛠+/="}}`, index, i)
		if err := a.observe(event); err != nil {
			t.Fatal(err)
		}
	}
	before, err := a.complete(json.RawMessage(`{"id":"r","future":1e1000,"output":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.observe([]byte(`{"type":"response.output_item.done","output_index":9223372036854775807,"item":{"id":"excess"}}`)); err == nil {
		t.Fatal("item count limit accepted an extra sparse item")
	}
	after, err := a.complete(json.RawMessage(`{"id":"r","future":1e1000,"output":[]}`))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("rejecting an extra item changed the collected response")
	}
	var decoded struct {
		Future json.RawMessage `json:"future"`
		Output []struct {
			ID     string          `json:"id"`
			Future json.RawMessage `json:"future"`
			Opaque string          `json:"opaque"`
		} `json:"output"`
	}
	if err := json.Unmarshal(after, &decoded); err != nil || len(decoded.Output) != maxCollectedResponseItems || string(decoded.Future) != "1e1000" {
		t.Fatalf("response fields changed: items=%d err=%v", len(decoded.Output), err)
	}
	for i, item := range decoded.Output {
		if item.ID != fmt.Sprintf("i%d", i) || string(item.Future) != "9007199254740993" || item.Opaque != "你好🛠+/=" {
			t.Fatalf("item %d lost order, number precision or UTF-8", i)
		}
	}
	// A full accumulator can still revise one existing item.
	if err := a.observe([]byte(`{"type":"response.output_item.done","output_index":0,"item":{"id":"i0","updated":true}}`)); err != nil {
		t.Fatal(err)
	}
}

func TestGatewayPerformanceAuditSSEEveryTwoCuts(t *testing.T) {
	wire := []byte("\ufeffdata: {\"type\":\"response.completed\",\r\ndata: \"response\":{\"id\":\"r\",\"unknown\":9007199254740993,\"opaque\":\"你好🛠\"}}\r\n\r\n")
	want, err := collectResponse(bytes.NewReader(wire))
	if err != nil {
		t.Fatal(err)
	}
	// Exhaustive two-cut partitions cross the BOM, Unicode, data field and CRLF.
	for first := 0; first <= len(wire); first++ {
		for second := first; second <= len(wire); second++ {
			reader := io.MultiReader(bytes.NewReader(wire[:first]), bytes.NewReader(wire[first:second]), bytes.NewReader(wire[second:]))
			got, err := collectResponse(reader)
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("cuts %d/%d changed output: %v", first, second, err)
			}
		}
	}
}

func TestGatewayPerformanceAuditIPCHeadBudget(t *testing.T) {
	const name = "X-Audit"
	// Base64 makes the encoded boundary smaller than the raw-header boundary.
	size := (gatewayNativeHeadLimit - len(name) - 8) / 4 * 3
	headers, err := gatewayNativeRequestHeaders(http.Header{name: {strings.Repeat("x", size)}})
	if err != nil || len(headers) != 1 {
		t.Fatalf("last encodable header bucket rejected: %v", err)
	}
	decoded, err := base64.StdEncoding.DecodeString(headers[0][1])
	if err != nil || len(decoded) != size {
		t.Fatalf("accepted header bytes changed: %v", err)
	}
	if _, err := gatewayNativeRequestHeaders(http.Header{name: {strings.Repeat("x", size+1)}}); err == nil {
		t.Fatal("encoded header exceeded the IPC budget")
	}
	var frame [5]byte
	frame[0] = 1
	binary.BigEndian.PutUint32(frame[1:], gatewayNativeHeadLimit+1)
	reader := bytes.NewReader(append(frame[:], []byte("must remain unread")...))
	if _, _, err := gatewayNativeReadFrame(reader); !errors.Is(err, errGatewayNativeProtocol) || reader.Len() != len("must remain unread") {
		t.Fatalf("oversized frame reached its payload: remaining=%d error=%v", reader.Len(), err)
	}
}

func TestGatewayPerformanceAuditLargeCompressionDoesNotEnterCache(t *testing.T) {
	// Nonparallel: all compression tests share this bounded cache.
	var cached []*zstd.Encoder
	for len(gatewayRequestEncoders) > 0 {
		cached = append(cached, <-gatewayRequestEncoders)
	}
	defer func() {
		for len(gatewayRequestEncoders) > 0 {
			_ = (<-gatewayRequestEncoders).Close()
		}
		for _, encoder := range cached {
			gatewayRequestEncoders <- encoder
		}
	}()
	r := httptest.NewRequest(http.MethodPost, "/responses", nil)
	r.Header.Set("Content-Encoding", "zstd")
	body := bytes.Repeat([]byte("x"), gatewayReusableCompressionBody+1)
	wire, _, err := prepareGatewayRequestBody(&Proxy{}, r, "/responses", body)
	if err != nil || len(gatewayRequestEncoders) != 0 {
		t.Fatalf("large encoder retained: cached=%d err=%v", len(gatewayRequestEncoders), err)
	}
	decoder, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
	if err != nil {
		t.Fatal(err)
	}
	defer decoder.Close()
	decoded, err := decoder.DecodeAll(wire, nil)
	if err != nil || !bytes.Equal(decoded, body) {
		t.Fatalf("large uncached output corrupted: %v", err)
	}
}

func TestGatewayPerformanceAuditCanceledCompressionLeavesCache(t *testing.T) {
	before := len(gatewayRequestEncoders)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	r := httptest.NewRequest(http.MethodPost, "/responses", nil).WithContext(ctx)
	r.Header.Set("Content-Encoding", "zstd")
	wire, native, err := prepareGatewayRequestBody(&Proxy{}, r, "/responses", []byte(`{"input":[]}`))
	if !errors.Is(err, context.Canceled) || wire != nil || native || len(gatewayRequestEncoders) != before {
		t.Fatalf("canceled preparation consumed resources: native=%v err=%v", native, err)
	}
}

func TestGatewayPerformanceAuditHeaderNormalizationIdempotence(t *testing.T) {
	source := http.Header{
		"User-Agent": {"codex_exec/0.155.0"}, "user-agent": {"ignored duplicate"},
		"X-Codex-Future": {"opaque+/=", "\xe9"}, "x-codex-future": {"third"},
		"Tracestate": {"first=one"}, "tracestate": {"second=two"},
		"Connection": {"X-Codex-Hop"}, "X-Codex-Hop": {"private"},
		"Authorization": {"synthetic-client-only"},
	}
	original := source.Clone()
	first, second := make(http.Header), make(http.Header)
	copyRequestHeaders(first, source)
	copyRequestHeaders(second, first)
	if !reflect.DeepEqual(first, second) || !reflect.DeepEqual(source, original) {
		t.Fatal("header normalization is not idempotent or mutated the caller")
	}
	if first.Get("X-Codex-Hop") != "" || first.Get("Authorization") != "" || len(first.Values("X-Codex-Future")) != 3 {
		t.Fatal("normalization lost end-to-end fields or retained hop/auth fields")
	}
}

func TestGatewayPerformanceAuditConcurrentCloseJoinsTimer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		raw := &auditPacedStream{chunks: []string{"data: pending"}, delay: time.Second, done: make(chan struct{})}
		ctx, cancel := context.WithCancel(t.Context())
		body := &idleStreamBody{body: raw, cancel: cancel, idle: time.Millisecond, sse: true}
		done := make(chan error, 1)
		go func() { _, err := body.Read(make([]byte, 16)); done <- err }()
		synctest.Wait()
		var closers sync.WaitGroup
		for range 32 {
			closers.Go(func() { _ = body.Close() })
		}
		closers.Wait()
		synctest.Wait()
		if err := <-done; !errors.Is(err, io.ErrClosedPipe) || ctx.Err() != context.Canceled || raw.closes.Load() != 1 {
			t.Fatalf("cancel/close lifecycle: closes=%d err=%v", raw.closes.Load(), err)
		}
		if body.timer == nil || body.timer.Stop() || len(body.expired) != 0 {
			t.Fatal("finished read left a timer or unjoined callback")
		}
	})
}

func TestGatewayPerformanceAuditProxyPortBoundaries(t *testing.T) {
	for _, host := range []string{"example.invalid", "[::1]"} {
		for _, port := range []string{"1", "00001", "443", "65535", "065535"} {
			got, err := gatewayNativeSOCKSAuthority(host+":"+port, "https")
			if err != nil {
				t.Fatalf("legal authority %s:%s rejected: %v", host, port, err)
			}
			second, err := gatewayNativeSOCKSAuthority(got, "https")
			if err != nil || second != got {
				t.Fatal("canonical proxy authority is unstable")
			}
		}
		for _, port := range []string{"", "0", "00000", "65536", "-1", "+443", "1e3", "0x50", "999999999999999999999999", "１２３"} {
			if _, err := gatewayNativeSOCKSAuthority(host+":"+port, "https"); err == nil {
				t.Fatalf("invalid port accepted: %q", port)
			}
		}
	}
}

// Segmentation must not create or hide an event, including on malformed UTF-8.
func FuzzGatewayAuditSSEChunkInvariant(f *testing.F) {
	f.Add([]byte("\ufeffdata: 你好🛠\r\n\r\n"), []byte{1, 2, 3})
	f.Add([]byte("data:\r\rx"), []byte{0})
	f.Add([]byte("data: \xff\n\ndata: {}\n\n"), []byte{255, 1})
	f.Fuzz(func(t *testing.T, wire, cuts []byte) {
		if len(wire) > 8192 || len(cuts) > 128 {
			t.Skip()
		}
		var whole, split gatewaySSEIdleProgress
		want := whole.consume(wire)
		got, offset := false, 0
		for _, size := range cuts {
			end := min(offset+int(size)+1, len(wire))
			got = split.consume(wire[offset:end]) || got
			offset = end
		}
		got = split.consume(wire[offset:]) || got
		if got != want || split != whole {
			t.Fatal("read chunk partition changed event progress")
		}
		// Compare the stateful collector splitter with the original stateless
		// split contract, while an independent scanner manages buffer shifts.
		chunk := 1
		if len(cuts) > 0 {
			chunk += int(cuts[0])
		}
		original := bufio.NewScanner(bytes.NewReader(wire))
		original.Split(splitSSELine)
		fragmented := bufio.NewScanner(performanceAuditChunkReader{Reader: bytes.NewReader(wire), chunk: chunk})
		var lines sseLineSplitter
		fragmented.Split(lines.split)
		var originalLines, fragmentedLines []string
		for original.Scan() {
			originalLines = append(originalLines, original.Text())
		}
		for fragmented.Scan() {
			fragmentedLines = append(fragmentedLines, fragmented.Text())
		}
		if original.Err() != nil || fragmented.Err() != nil || !reflect.DeepEqual(originalLines, fragmentedLines) {
			t.Fatal("stateful line scanning changed framing across fragmented reads")
		}
	})
}

func FuzzGatewayAuditNativeIPCFrames(f *testing.F) {
	f.Add(uint8(1), uint32(2), []byte("{}"))
	f.Add(uint8(2), uint32(65537), []byte("unread"))
	f.Add(uint8(3), uint32(0), []byte{})
	f.Add(uint8(4), uint32(8), []byte("short"))
	f.Fuzz(func(t *testing.T, kind uint8, declared uint32, payload []byte) {
		if len(payload) > 8192 {
			t.Skip()
		}
		var head [5]byte
		head[0] = kind
		binary.BigEndian.PutUint32(head[1:], declared)
		wire := append(head[:], payload...)
		gotKind, data, err := gatewayNativeReadFrame(iotest.OneByteReader(bytes.NewReader(wire)))
		legal := kind >= 1 && kind <= 4 && declared <= gatewayNativeHeadLimit && (kind != 3 || declared == 0)
		if !legal {
			if !errors.Is(err, errGatewayNativeProtocol) || data != nil {
				t.Fatal("invalid frame became a data-bearing result")
			}
		} else if uint32(len(payload)) < declared {
			if !errors.Is(err, io.ErrUnexpectedEOF) || data != nil {
				t.Fatal("truncated IPC frame became success")
			}
		} else if err != nil || gotKind != kind || !bytes.Equal(data, payload[:declared]) {
			t.Fatalf("legal fragmented frame changed: %v", err)
		}
	})
}

func BenchmarkGatewayPerformanceAuditAggregation(b *testing.B) {
	for _, count := range []int{1, 128, 8192} {
		b.Run(fmt.Sprintf("items=%d", count), func(b *testing.B) {
			events := make([][]byte, count)
			for i := range events {
				events[i] = fmt.Appendf(nil, `{"type":"response.output_item.done","output_index":%d,"item":{"id":"item%d","type":"function_call","call_id":"call%d","arguments":"{}"}}`, int64(i)*(1<<40), i, i)
			}
			b.ReportAllocs()
			for b.Loop() {
				var a responseOutputAccumulator
				for _, event := range events {
					if err := a.observe(event); err != nil {
						b.Fatal(err)
					}
				}
				if _, err := a.complete(json.RawMessage(`{"id":"r","output":[]}`)); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkGatewayPerformanceAuditSSEState(b *testing.B) {
	for _, size := range []int{4096, 65536} {
		b.Run(fmt.Sprintf("bytes=%d", size), func(b *testing.B) {
			wire := []byte("data: " + strings.Repeat("x", size-8) + "\n\n")
			b.SetBytes(int64(len(wire)))
			b.ReportAllocs()
			var progress gatewaySSEIdleProgress
			for b.Loop() {
				if !progress.consume(wire) {
					b.Fatal("event missing")
				}
			}
		})
	}
}

func BenchmarkGatewayPerformanceAuditFragmentedLine(b *testing.B) {
	for _, size := range []int{1 << 20, 4 << 20, 32 << 20} {
		for _, stateful := range []bool{false, true} {
			b.Run(fmt.Sprintf("bytes=%d/stateful=%v", size, stateful), func(b *testing.B) {
				wire := []byte(strings.Repeat("x", size-2) + "\r\n")
				b.SetBytes(int64(size))
				b.ReportAllocs()
				for b.Loop() {
					scanner := bufio.NewScanner(performanceAuditChunkReader{Reader: bytes.NewReader(wire), chunk: 64 << 10})
					scanner.Buffer(make([]byte, 64<<10), maxBody)
					var lines sseLineSplitter
					if stateful {
						scanner.Split(lines.split)
					} else {
						scanner.Split(splitSSELine)
					}
					if !scanner.Scan() || len(scanner.Bytes()) != size-2 || scanner.Scan() || scanner.Err() != nil {
						b.Fatalf("fragmented scan lost bytes: %v", scanner.Err())
					}
				}
			})
		}
	}
}
