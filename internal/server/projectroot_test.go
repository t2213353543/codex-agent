package server

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// projectRoot locates checked-in test helpers without changing the process cwd.
// Walking up from cwd also works for copied snapshots and trimpath test builds.
func projectRoot(t *testing.T) string {
	t.Helper()
	var candidates []string
	if cwd, err := os.Getwd(); err == nil {
		candidates = append(candidates, cwd)
	}
	if _, file, _, ok := runtime.Caller(0); ok && filepath.IsAbs(file) {
		candidates = append(candidates, filepath.Dir(file))
	}
	for _, start := range candidates {
		for dir := start; ; dir = filepath.Dir(dir) {
			if info, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil && !info.IsDir() {
				return dir
			}
			if filepath.Dir(dir) == dir {
				break
			}
		}
	}
	t.Fatal("cannot find project root containing go.mod")
	return ""
}
