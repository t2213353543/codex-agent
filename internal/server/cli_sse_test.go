package server

import (
	"bytes"
	"strings"
	"testing"
)

func TestCLISSEObserverLineEndingsAndFragmentation(t *testing.T) {
	const payload = `{"type":"response.completed","response":{"id":"resp_cli","status":"completed","usage":{"input_tokens":23,"output_tokens":7}}}`
	for name, ending := range map[string]string{"LF": "\n", "CRLF": "\r\n", "CR": "\r"} {
		t.Run(name, func(t *testing.T) {
			wire := ": heartbeat" + ending + ending + "data: " + payload + ending + ending
			for split := 0; split <= len(wire); split++ {
				rec := RequestRecord{}
				scanner := sseScanner{rec: &rec}
				for _, chunk := range []string{wire[:split], wire[split:]} {
					if n, err := scanner.Write([]byte(chunk)); n != len(chunk) || err != nil {
						t.Fatalf("split %d: Write = %d, %v", split, n, err)
					}
				}
				scanner.Finish()
				if !scanner.finished || rec.Status != "ok" || rec.InputTokens != 23 || rec.OutputTokens != 7 {
					t.Fatalf("split %d: %+v", split, rec)
				}
			}
		})
	}
}

func TestCLISSEObserverRequiresBlankLineBeforeEOF(t *testing.T) {
	const payload = `data: {"type":"response.completed","response":{"id":"resp_unfinished","usage":{"input_tokens":99}}}`
	for name, ending := range map[string]string{"none": "", "LF": "\n", "CRLF": "\r\n", "CR": "\r"} {
		t.Run(name, func(t *testing.T) {
			rec := RequestRecord{}
			scanner := sseScanner{rec: &rec}
			_, _ = scanner.Write([]byte(payload + ending))
			scanner.Finish()
			scanner.Finish()
			if scanner.finished || rec.Status != "aborted" || rec.InputTokens != 0 || !strings.Contains(rec.Error, "without a terminal") {
				t.Fatalf("unterminated event counted as a completion: %+v", rec)
			}
			if scanner.line != nil || scanner.eventData != nil {
				t.Fatal("EOF retained incomplete event buffers")
			}
			_, _ = scanner.Write([]byte("\n\ndata: {\"type\":\"response.completed\",\"response\":{}}\n\n"))
			if rec.Status != "aborted" {
				t.Fatal("write after Finish changed the outcome")
			}
		})
	}
}

func TestCLISSEObserverCompletedReplacesDeferredIncomplete(t *testing.T) {
	rec := RequestRecord{}
	scanner := sseScanner{rec: &rec}
	_, _ = scanner.Write([]byte("data: " + `{"type":"response.incomplete","response":{"incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":13,"output_tokens":5}}}` + "\n\n"))
	_, _ = scanner.Write([]byte("data: " + `{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":100}}}` + "\n\n"))
	scanner.Finish()
	if rec.Status != "ok" || rec.Error != "" || rec.TerminalStatus != "completed" || rec.InputTokens != 100 || rec.OutputTokens != 0 {
		t.Fatalf("deferred incomplete contaminated completion: %+v", rec)
	}
}

func TestCLISSEObserverRejectsOversizedWriteBeforeBuffering(t *testing.T) {
	rec := RequestRecord{}
	scanner := sseScanner{rec: &rec}
	chunk := bytes.Repeat([]byte{'x'}, maxBody+1)
	if n, err := scanner.Write(chunk); n != len(chunk) || err != nil {
		t.Fatalf("observer interfered with forwarding: n=%d err=%v", n, err)
	}
	if rec.Status != "error" || !strings.Contains(rec.Error, "exceeds") || scanner.line != nil || scanner.eventData != nil {
		t.Fatalf("oversized write retained buffers or was accepted: %+v", rec)
	}
}

func TestCLISSEObserverBoundsCombinedPendingLineAndEvent(t *testing.T) {
	rec := RequestRecord{}
	scanner := sseScanner{rec: &rec}
	line := append([]byte("data: "), bytes.Repeat([]byte{' '}, maxBody/2)...)
	line = append(line, '\n')
	_, _ = scanner.Write(line)
	if rec.Status != "" {
		t.Fatalf("first bounded line rejected: %+v", rec)
	}
	_, _ = scanner.Write(line[:len(line)-1])
	if rec.Status != "error" || !strings.Contains(rec.Error, "exceeds") || scanner.line != nil || scanner.eventData != nil {
		t.Fatalf("pending data exceeded the event budget: %+v", rec)
	}
}
