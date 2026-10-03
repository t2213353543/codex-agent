// Standalone synthetic probe: Go's proven WS server drives a sealed Rust helper.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

const eventCount = 20
const eventPadding = 256 << 10

type result struct {
	Name                   string `json:"name"`
	Passed                 bool   `json:"passed"`
	Failure                string `json:"failure,omitempty"`
	Messages               int    `json:"messages"`
	MessageOrderExact      bool   `json:"message_order_exact"`
	AckObserved            bool   `json:"ack_observed"`
	ToolReceivedBeforeRead bool   `json:"tool_received_before_resume"`
	UpstreamWrites         int32  `json:"upstream_writes"`
	CancelMilliseconds     int64  `json:"cancel_milliseconds,omitempty"`
	AckAfterResumeMs       int64  `json:"ack_after_resume_ms,omitempty"`
	PartialPayloadBytes    int    `json:"blocked_frame_payload_bytes"`
	HandshakeCount         int32  `json:"handshake_count"`
}

type helper struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	done  chan error
	conn  *net.UnixConn
	dir   string
}

func writeJSON(w io.Writer, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if err := binary.Write(w, binary.BigEndian, uint32(len(raw))); err != nil {
		return err
	}
	_, err = w.Write(raw)
	return err
}

func frame(w io.Writer, kind byte, raw []byte) error {
	head := [5]byte{kind}
	binary.BigEndian.PutUint32(head[1:], uint32(len(raw)))
	if _, err := w.Write(head[:]); err != nil {
		return err
	}
	_, err := w.Write(raw)
	return err
}

func readHead(r io.Reader) (byte, int, error) {
	var head [5]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return 0, 0, err
	}
	n := int(binary.BigEndian.Uint32(head[1:]))
	if n > 32<<20+9 {
		return 0, 0, errors.New("frame_limit")
	}
	return head[0], n, nil
}

func readPayload(r io.Reader, n int) ([]byte, error) {
	data := make([]byte, n)
	_, err := io.ReadFull(r, data)
	return data, err
}

func startHelper(binaryPath, origin string, writeMS int) (*helper, error) {
	dir, err := os.MkdirTemp("", "ws-bp-")
	if err != nil {
		return nil, err
	}
	h := &helper{dir: dir, done: make(chan error, 1)}
	secret := make([]byte, 32)
	if _, err = rand.Read(secret); err != nil {
		return nil, err
	}
	token := hex.EncodeToString(secret)
	h.cmd = exec.Command(binaryPath)
	h.cmd.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
	h.cmd.Stderr = io.Discard
	h.stdin, err = h.cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := h.cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err = h.cmd.Start(); err != nil {
		return nil, err
	}
	go func() { h.done <- h.cmd.Wait() }()
	socket := filepath.Join(dir, "s")
	if err = writeJSON(h.stdin, map[string]any{"protocol": 1, "unix_socket": socket, "upstream": origin,
		"ipc_token": token, "headers_timeout_ms": 2000, "idle_timeout_ms": 0}); err != nil {
		h.close()
		return nil, err
	}
	var ready map[string]any
	line, err := bufio.NewReader(stdout).ReadBytes('\n')
	if err != nil || json.Unmarshal(line, &ready) != nil || ready["transport"] != "unix" {
		h.close()
		return nil, errors.New("helper_ready")
	}
	h.conn, err = net.DialUnix("unix", nil, &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		h.close()
		return nil, err
	}
	_ = h.conn.SetReadBuffer(4096)
	_ = h.conn.SetDeadline(time.Now().Add(12 * time.Second))
	err = writeJSON(h.conn, map[string]any{"protocol": 1, "ipc_token": token, "operation": "websocket_message_v1",
		"method": "GET", "path": "/responses", "headers": [][2]string{}, "body_length": 0, "write_timeout_ms": writeMS})
	if err != nil {
		h.close()
		return nil, err
	}
	kind, n, err := readHead(h.conn)
	if err != nil || kind != 65 {
		h.close()
		return nil, errors.New("handshake_frame")
	}
	raw, err := readPayload(h.conn, n)
	var response struct{ Status int }
	if err != nil || json.Unmarshal(raw, &response) != nil || response.Status != 101 {
		h.close()
		return nil, errors.New("handshake_status")
	}
	return h, nil
}

func (h *helper) close() {
	if h.conn != nil {
		_ = h.conn.Close()
	}
	if h.stdin != nil {
		_ = h.stdin.Close()
	}
	select {
	case <-h.done:
	case <-time.After(3 * time.Second):
		_ = h.cmd.Process.Kill()
		<-h.done
	}
	_ = os.Remove(h.dir)
}

func runCase(binaryPath, name string) (out result) {
	out.Name = name
	var handshakes, writes atomic.Int32
	toolSeen, peerClosed := make(chan struct{}), make(chan struct{})
	var seenOnce, closedOnce sync.Once
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionContextTakeover})
		if err != nil {
			return
		}
		handshakes.Add(1)
		defer conn.CloseNow()
		defer closedOnce.Do(func() { close(peerClosed) })
		go func() {
			for index := 0; index < eventCount; index++ {
				data, _ := json.Marshal(map[string]any{"event_index": index, "padding": strings.Repeat("q", eventPadding)})
				if conn.Write(ctx, websocket.MessageText, data) != nil {
					return
				}
				writes.Add(1)
			}
		}()
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			if string(data) == `{"type":"function_call_output","output":"synthetic"}` {
				seenOnce.Do(func() { close(toolSeen) })
			}
		}
	}))
	defer server.Close()
	writeMS := 5000
	if name == "bounded_write_timeout" {
		writeMS = 400
	}
	h, err := startHelper(binaryPath, server.URL, writeMS)
	if err != nil {
		out.Failure = "start_failed"
		return
	}
	defer h.close()
	defer func() { out.HandshakeCount, out.UpstreamWrites = handshakes.Load(), writes.Load() }()
	kind, n, err := readHead(h.conn)
	if err != nil || kind != 66 || n < eventPadding {
		out.Failure = "first_large_message_missing"
		return
	}
	out.PartialPayloadBytes = n
	// The first full message greatly exceeds the IPC receive buffer. Read only
	// its prefix, then leave the payload blocked while writing in reverse.
	time.Sleep(150 * time.Millisecond)
	data := make([]byte, 9)
	binary.BigEndian.PutUint64(data, 1)
	data[8] = 1
	data = append(data, []byte(`{"type":"function_call_output","output":"synthetic"}`)...)
	if frame(h.conn, 82, data) != nil {
		out.Failure = "send_failed"
		return
	}
	time.Sleep(150 * time.Millisecond)
	select {
	case <-toolSeen:
		out.ToolReceivedBeforeRead = true
	default:
	}
	if name != "resume_consumer" {
		began := time.Now()
		switch name {
		case "cancel_frame":
			if frame(h.conn, 84, nil) != nil {
				out.Failure = "cancel_write_failed"
				return
			}
		case "parent_stdin_eof":
			_ = h.stdin.Close()
		case "bounded_write_timeout":
		}
		select {
		case <-peerClosed:
			out.CancelMilliseconds = time.Since(began).Milliseconds()
			out.Passed = out.CancelMilliseconds < 1500 && handshakes.Load() == 1
		case <-time.After(2 * time.Second):
			out.Failure = "upstream_not_cancelled"
		}
		return
	}
	resumed := time.Now()
	for out.Messages < eventCount || !out.AckObserved {
		raw, err := readPayload(h.conn, n)
		if err != nil {
			out.Failure = "incomplete_frame"
			return
		}
		switch kind {
		case 66:
			var message struct {
				EventIndex int    `json:"event_index"`
				Padding    string `json:"padding"`
			}
			if len(raw) < 9 || binary.BigEndian.Uint64(raw[:8]) != uint64(out.Messages+1) || raw[8] != 1 || json.Unmarshal(raw[9:], &message) != nil || message.EventIndex != out.Messages || len(message.Padding) != eventPadding {
				out.Failure = "message_order_or_content"
				return
			}
			out.Messages++
		case 70:
			if len(raw) != 8 || binary.BigEndian.Uint64(raw) != 1 || out.AckObserved {
				out.Failure = "wrong_or_duplicate_ack"
				return
			}
			out.AckObserved = true
			out.AckAfterResumeMs = time.Since(resumed).Milliseconds()
		default:
			out.Failure = fmt.Sprintf("unexpected_frame_%d", kind)
			return
		}
		if out.Messages == eventCount && out.AckObserved {
			break
		}
		kind, n, err = readHead(h.conn)
		if err != nil {
			out.Failure = "missing_next_frame"
			return
		}
	}
	select {
	case <-toolSeen:
	case <-time.After(time.Second):
		out.Failure = "tool_not_received"
		return
	}
	out.MessageOrderExact = true
	out.Passed = out.AckObserved && out.Messages == eventCount && handshakes.Load() == 1
	return
}

func main() {
	binaryPath, reportPath := flag.String("helper", "", "sealed helper"), flag.String("report", "", "sanitized evidence")
	flag.Parse()
	raw, err := os.ReadFile(*binaryPath)
	if err != nil {
		panic("helper unavailable")
	}
	sum := sha256.Sum256(raw)
	cases := []result{}
	passed := true
	for _, name := range []string{"resume_consumer", "cancel_frame", "parent_stdin_eof", "bounded_write_timeout"} {
		out := runCase(*binaryPath, name)
		cases = append(cases, out)
		passed = passed && out.Passed
	}
	report := map[string]any{"schema": "sealed-rust-ws-backpressure-v1", "helper_sha256": hex.EncodeToString(sum[:]),
		"server_library": "github.com/coder/websocket v1.8.14", "network": "none", "live_accounts": false,
		"production_code_modified": false, "messages_per_case": eventCount, "message_padding_bytes": eventPadding,
		"ipc_receive_buffer_requested": 4096, "cases": cases, "passed": passed}
	encoded, _ := json.MarshalIndent(report, "", "  ")
	if os.WriteFile(*reportPath, encoded, 0600) != nil {
		panic("cannot write report")
	}
	fmt.Println(string(encoded))
	if !passed {
		os.Exit(1)
	}
}
