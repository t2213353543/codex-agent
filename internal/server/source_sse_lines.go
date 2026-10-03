package server

import (
	"bytes"
	"unicode/utf8"
)

func validSSELine(line []byte) bool { return utf8.Valid(line) }

// stripInitialSSEBOM removes exactly one UTF-8 BOM at the start of the stream.
// A BOM on a later line belongs to that line's field name and is not stripped.
// Call only with complete lines, so a BOM split across reads is handled once.
func stripInitialSSEBOM(line []byte, started *bool) []byte {
	if !*started {
		*started = true
		return bytes.TrimPrefix(line, []byte{0xef, 0xbb, 0xbf})
	}
	return line
}

// splitSSELine recognizes the same CR, LF and CRLF event boundaries as the
// native Codex eventsource-stream parser. A trailing CR waits for the next
// read so a split CRLF never becomes two line endings.
func splitSSELine(data []byte, atEOF bool) (advance int, token []byte, err error) {
	var splitter sseLineSplitter
	return splitter.split(data, atEOF)
}

// Scanner can call its split function hundreds of times while accumulating a
// single large line. Retain the checked prefix so fragmented input is scanned
// once instead of repeatedly walking all bytes collected so far.
type sseLineSplitter struct{ scanned int }

func (s *sseLineSplitter) split(data []byte, atEOF bool) (advance int, token []byte, err error) {
	start := min(s.scanned, len(data))
	if relative := bytes.IndexAny(data[start:], "\r\n"); relative >= 0 {
		index := start + relative
		advance = index + 1
		if data[index] == '\r' {
			if advance == len(data) && !atEOF {
				s.scanned = index // Revisit only the CR when its lookahead arrives.
				return 0, nil, nil
			}
			if advance < len(data) && data[advance] == '\n' {
				advance++
			}
		}
		s.scanned = 0
		return advance, data[:index], nil
	}
	s.scanned = len(data)
	if atEOF && len(data) > 0 {
		s.scanned = 0
		return len(data), data, nil
	}
	return 0, nil, nil
}
