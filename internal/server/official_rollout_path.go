package server

import (
	"errors"
	"os"
	"path/filepath"
)

// A thread may name its rollout before creating the file. Resolve the nearest
// existing ancestor so a missing leaf cannot hide an escaping parent symlink.
// A dangling or unreadable symlink is an error, never an unchecked lexical path.
func officialResolveRolloutPath(path string) (string, error) {
	candidate := path
	missing := []string{}
	for {
		real, err := filepath.EvalSymlinks(candidate)
		if err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				real = filepath.Join(real, missing[i])
			}
			return real, nil
		}
		if !os.IsNotExist(err) {
			return "", errors.New("official rollout path cannot be resolved")
		}
		if info, lstatErr := os.Lstat(candidate); lstatErr == nil && info.Mode()&os.ModeSymlink != 0 || lstatErr != nil && !os.IsNotExist(lstatErr) {
			return "", errors.New("official rollout path contains an unavailable link")
		}
		parent := filepath.Dir(candidate)
		if parent == candidate {
			return "", errors.New("official rollout path has no accessible ancestor")
		}
		missing = append(missing, filepath.Base(candidate))
		candidate = parent
	}
}
