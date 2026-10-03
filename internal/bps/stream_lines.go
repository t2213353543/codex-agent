package bps

import (
	"bufio"
	"bytes"
	"io"
)

type streamLines interface {
	Scan() bool
	Bytes() []byte
	Err() error
}

func newStreamLines(reader io.Reader, options StreamOptions) streamLines {
	if options.Unbounded {
		return &unboundedStreamLines{reader: reader}
	}
	scanner := bufio.NewScanner(reader)
	splitter := &streamLineSplitter{}
	scanner.Split(splitter.split)
	scanner.Buffer(make([]byte, min(4<<10, options.MaxEventBytes+1)), options.MaxEventBytes+1)
	return scanner
}

// Incremental CR/LF/CRLF reader retains only one line, with no Scanner token
// ceiling. Looking solely for LF would stall a valid CR-only live stream.
type unboundedStreamLines struct {
	reader        io.Reader
	buffer        [64 << 10]byte
	pending, line []byte
	err           error
	skipLF        bool
}

func (s *unboundedStreamLines) Scan() bool {
	s.line = s.line[:0]
	emptyReads := 0
	for {
		if len(s.pending) > 0 {
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
			return len(s.line) > 0
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
func (s *unboundedStreamLines) Bytes() []byte { return s.line }
func (s *unboundedStreamLines) Err() error {
	if s.err == io.EOF {
		return nil
	}
	return s.err
}
