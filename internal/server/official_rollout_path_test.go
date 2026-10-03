package server

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestOfficialRolloutPathCanonicalRuntimeAlias(t *testing.T) {
	root := t.TempDir()
	private := filepath.Join(root, "private", "codex-agent")
	home := filepath.Join(private, "official-runtime", "session-one", "home")
	if err := os.MkdirAll(filepath.Join(home, "sessions"), 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "codex-agent")
	if err := os.Symlink(private, alias); err != nil {
		if runtime.GOOS == "windows" {
			t.Skip("Windows symlink privilege unavailable")
		}
		t.Fatal(err)
	}
	aliasHome := filepath.Join(alias, "official-runtime", "session-one", "home")
	realFile := filepath.Join(home, "sessions", "rollout.jsonl")
	if err := os.WriteFile(realFile, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	canonicalFile, err := filepath.EvalSymlinks(realFile)
	if err != nil {
		t.Fatal(err)
	}
	for name, pair := range map[string][2]string{
		"alias home resolved rollout": {aliasHome, realFile},
		"resolved home alias rollout": {home, filepath.Join(aliasHome, "sessions", "rollout.jsonl")},
		"both alias":                  {aliasHome, filepath.Join(aliasHome, "sessions", "rollout.jsonl")},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := officialRolloutPath(pair[0], pair[1])
			if err != nil || got != canonicalFile {
				t.Fatalf("managed runtime alias rejected: %q %v", got, err)
			}
		})
	}
	missing := filepath.Join(home, "sessions", "new", "rollout.jsonl")
	canonicalHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := officialRolloutPath(aliasHome, missing); err != nil || got != filepath.Join(canonicalHome, "sessions", "new", "rollout.jsonl") {
		t.Fatalf("new rollout within canonical home rejected: %q %v", got, err)
	}
	outside := filepath.Join(root, "other-session")
	if err := os.Mkdir(outside, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "existing.jsonl"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	redirect := filepath.Join(home, "redirect")
	if err := os.Symlink(outside, redirect); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(home, "dangling")
	if err := os.Symlink(filepath.Join(root, "does-not-exist"), dangling); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{home, outside, filepath.Join(redirect, "existing.jsonl"), filepath.Join(redirect, "missing.jsonl"), filepath.Join(dangling, "missing.jsonl"), filepath.Join(root, "home-neighbor", "rollout.jsonl")} {
		if _, err := officialRolloutPath(aliasHome, path); err == nil {
			t.Fatalf("rollout outside runtime home accepted: %q", path)
		}
	}
}

func TestOfficialRolloutPathRejectsMissingTrustedHome(t *testing.T) {
	root := t.TempDir()
	if _, err := officialRolloutPath(filepath.Join(root, "missing-home"), filepath.Join(root, "missing-home", "rollout.jsonl")); err == nil {
		t.Fatal("missing trusted runtime home accepted")
	}
}
