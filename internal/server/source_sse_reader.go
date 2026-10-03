package server

import (
	"bufio"
	"bytes"
	"io"
)

type gatewaySSELines interface {
	Scan() bool
	Bytes() []byte
	Err() error
}

func newGatewaySSELines(reader io.Reader, unbounded bool) gatewaySSELines {
	if unbounded {
		return &gatewayUnboundedSSELines{reader: reader}
	}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), maxBody)
	var splitter sseLineSplitter
	scanner.Split(splitter.split)
	return scanner
}

// Read in fixed chunks while retaining only the current line. Unlike Scanner,
// the response path has no configured token ceiling. CR alone is also a valid
// SSE separator, so waiting for ReadSlice('\n') could stall a live CR stream.
type gatewayUnboundedSSELines struct {
	reader  io.Reader
	buffer  [64 << 10]byte
	pending []byte
	line    []byte
	err     error
	skipLF  bool
}

func (s *gatewayUnboundedSSELines) Scan() bool {
	s.line = s.line[:0]
	emptyReads := 0
	for {
		if len(s.pending) != 0 {
			if s.skipLF {
				s.skipLF = false
				if s.pending[0] == '\n' {
					s.pending = s.pending[1:]
				}
			}
			if at := bytes.IndexAny(s.pending, "\r\n"); at >= 0 {
				s.line = append(s.line, s.pending[:at]...)
				s.skipLF = s.pending[at] == '\r'
				s.pending = s.pending[at+1:]
				return true
			}
			s.line = append(s.line, s.pending...)
			s.pending = nil
		}
		if s.err != nil {
			return len(s.line) != 0
		}
		n, err := s.reader.Read(s.buffer[:])
		s.pending, s.err = s.buffer[:n], err
		if n == 0 && err == nil {
			emptyReads++
			if emptyReads == 100 {
				s.err = io.ErrNoProgress
			}
		} else {
			emptyReads = 0
		}
	}
}

func (s *gatewayUnboundedSSELines) Bytes() []byte { return s.line }
func (s *gatewayUnboundedSSELines) Err() error {
	if s.err == io.EOF {
		return nil
	}
	return s.err
}
