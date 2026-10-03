package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

// File-mode exports use external merge sort so they obey the same order as
// the list without keeping every retained request (or every open run) in RAM.
func (m *Metrics) walkManagedLogsOrdered(ctx context.Context, q managedLogQuery, visit func(RequestRecord) error) error {
	if m.db != nil {
		return m.walkManagedLogs(ctx, q, visit)
	}
	folder, err := os.MkdirTemp("", "codex-usage-sort-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(folder)
	next := 0
	newPath := func() string { next++; return filepath.Join(folder, fmt.Sprintf("run-%d.jsonl", next)) }
	var runs []string
	batch := make([]RequestRecord, 0, 512)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		sort.Slice(batch, func(i, j int) bool { return q.before(batch[i], batch[j]) })
		name := newPath()
		file, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		buffer := bufio.NewWriter(file)
		enc := json.NewEncoder(buffer)
		for _, record := range batch {
			if err = enc.Encode(record); err != nil {
				file.Close()
				return err
			}
		}
		if err = buffer.Flush(); err != nil {
			file.Close()
			return err
		}
		if err = file.Close(); err != nil {
			return err
		}
		runs = append(runs, name)
		batch = batch[:0]
		return nil
	}
	_, err = m.walkManagedFile(ctx, q.FileCeiling, func(r RequestRecord) error {
		if q.matches(r) {
			batch = append(batch, r)
			if len(batch) == cap(batch) {
				return flush()
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err = flush(); err != nil {
		return err
	}
	for len(runs) > 1 {
		merged := []string{}
		for i := 0; i < len(runs); i += 2 {
			if i+1 == len(runs) {
				merged = append(merged, runs[i])
				continue
			}
			target := newPath()
			if err = mergeManagedRuns(ctx, q, runs[i], runs[i+1], target); err != nil {
				return err
			}
			if err = os.Remove(runs[i]); err != nil {
				return err
			}
			if err = os.Remove(runs[i+1]); err != nil {
				return err
			}
			merged = append(merged, target)
		}
		runs = merged
	}
	if len(runs) == 0 {
		return nil
	}
	file, err := os.Open(runs[0])
	if err != nil {
		return err
	}
	defer file.Close()
	decoder := json.NewDecoder(bufio.NewReader(file))
	for {
		if err = ctx.Err(); err != nil {
			return err
		}
		var r RequestRecord
		err = decoder.Decode(&r)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err = visit(r); err != nil {
			return err
		}
	}
}

func mergeManagedRuns(ctx context.Context, q managedLogQuery, left, right, target string) error {
	a, err := os.Open(left)
	if err != nil {
		return err
	}
	defer a.Close()
	b, err := os.Open(right)
	if err != nil {
		return err
	}
	defer b.Close()
	output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer output.Close()
	buffer := bufio.NewWriter(output)
	encoder := json.NewEncoder(buffer)
	da, db := json.NewDecoder(bufio.NewReader(a)), json.NewDecoder(bufio.NewReader(b))
	var ra, rb RequestRecord
	ea, eb := da.Decode(&ra), db.Decode(&rb)
	for ea == nil || eb == nil {
		if err = ctx.Err(); err != nil {
			return err
		}
		if ea != nil && ea != io.EOF {
			return ea
		}
		if eb != nil && eb != io.EOF {
			return eb
		}
		if eb != nil || ea == nil && q.before(ra, rb) {
			if err = encoder.Encode(ra); err != nil {
				return err
			}
			ea = da.Decode(&ra)
		} else {
			if err = encoder.Encode(rb); err != nil {
				return err
			}
			eb = db.Decode(&rb)
		}
	}
	if ea != io.EOF {
		return ea
	}
	if eb != io.EOF {
		return eb
	}
	if err = buffer.Flush(); err != nil {
		return err
	}
	return output.Close()
}
