package server

import (
	"bufio"
	"bytes"
	"io"
)

// Official stdout is newline-delimited JSON. ReadBytes grows to the actual
// message size instead of terminating a healthy task at a scanner token limit.
// A final non-newline-terminated record is still delivered before EOF.
func officialReadJSONLines(reader io.Reader, visit func([]byte) error) error {
	buffer := bufio.NewReader(reader)
	for {
		line, err := buffer.ReadBytes('\n')
		line = bytes.TrimSuffix(line, []byte{'\n'})
		line = bytes.TrimSuffix(line, []byte{'\r'})
		if len(line) > 0 {
			if visitErr := visit(line); visitErr != nil {
				return visitErr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
