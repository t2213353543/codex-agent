package server

import "unicode/utf8"

// gatewaySSEIdleProgress tracks eventsource-stream's dispatch boundary without
// retaining event data or altering any response bytes. An event needs a data
// field (including an empty one) followed by an empty line. Comments and fields
// such as id/event/retry alone do not yield an event in the CLI's parser.
type gatewaySSEIdleProgress struct {
	started       bool
	invalidUTF8   bool
	runeBytes     [utf8.UTFMax]byte
	runeLen       uint8
	pendingCR     bool
	lineNonempty  bool
	fieldLen      uint8
	fieldMismatch bool
	inValue       bool
	dataField     bool
	hasData       bool
}

func (s *gatewaySSEIdleProgress) consume(data []byte) bool {
	completed := false
	for _, b := range data {
		if s.invalidUTF8 {
			break
		}
		if s.runeLen == 0 && b < utf8.RuneSelf {
			s.started = true
			completed = s.consumeByte(b) || completed
			continue
		}
		s.runeBytes[s.runeLen] = b
		s.runeLen++
		if !utf8.FullRune(s.runeBytes[:s.runeLen]) {
			continue
		}
		r, size := utf8.DecodeRune(s.runeBytes[:s.runeLen])
		if r == utf8.RuneError && size == 1 {
			// Like Utf8Stream, an invalid prefix prevents parsing later bytes.
			s.invalidUTF8 = true
			continue
		}
		s.runeLen = 0
		initial := !s.started
		s.started = true
		if initial && r == '\ufeff' {
			continue
		}
		// All non-ASCII runes are ordinary field/value characters; their
		// exact value cannot affect a data-field or line-boundary match.
		completed = s.consumeByte(utf8.RuneSelf) || completed
	}
	return completed
}

func (s *gatewaySSEIdleProgress) consumeByte(b byte) bool {
	completed := false
	if s.pendingCR {
		s.pendingCR = false
		completed = s.finishLine()
		if b == '\n' {
			return completed
		}
	}
	switch b {
	case '\r':
		// nom's streaming CRLF parser needs lookahead before accepting CR.
		s.pendingCR = true
	case '\n':
		completed = s.finishLine() || completed
	default:
		s.lineNonempty = true
		if !s.inValue {
			if b == ':' {
				s.inValue = true
				s.dataField = s.fieldLen == 4 && !s.fieldMismatch
			} else {
				if s.fieldLen >= 4 || b != "data"[s.fieldLen] {
					s.fieldMismatch = true
				}
				if s.fieldLen < 5 {
					s.fieldLen++
				}
			}
		}
	}
	return completed
}

func (s *gatewaySSEIdleProgress) finishLine() bool {
	completed := false
	if !s.lineNonempty {
		completed, s.hasData = s.hasData, false
	} else if s.dataField || (!s.inValue && s.fieldLen == 4 && !s.fieldMismatch) {
		s.hasData = true
	}
	s.lineNonempty, s.inValue, s.dataField, s.fieldMismatch = false, false, false, false
	s.fieldLen = 0
	return completed
}
