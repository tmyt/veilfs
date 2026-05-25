package ignore

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestMatcher_BasicPatterns(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, ".veilignore")
	writeFile(t, cfg, "secret.env\nsecrets/\n*.log\n!important.log\n")

	m, err := NewLiveMatcher(cfg, dir, t.Logf)
	if err != nil {
		t.Fatalf("NewLiveMatcher: %v", err)
	}

	cases := []struct {
		path  string
		isDir bool
		want  bool
	}{
		{"safe.txt", false, false},
		{"secret.env", false, true},
		{"secrets", true, true},
		{"secrets/key.pem", false, true},
		{"secrets/nested/deep.bin", false, true},
		{"debug.log", false, true},
		{"important.log", false, false},
		{".veilignore", false, true},
		{"sub/.veilignore", false, true},
	}
	for _, tc := range cases {
		got := m.Match(tc.path, tc.isDir)
		if got != tc.want {
			t.Errorf("Match(%q, isDir=%v) = %v, want %v", tc.path, tc.isDir, got, tc.want)
		}
	}
}

func TestMatcher_MissingFileStartsEmpty(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, ".veilignore")

	m, err := NewLiveMatcher(cfg, dir, t.Logf)
	if err != nil {
		t.Fatalf("NewLiveMatcher: %v", err)
	}
	if m.Match("anything.txt", false) {
		t.Errorf("empty matcher must not hide arbitrary files")
	}
	if !m.Match(".veilignore", false) {
		t.Errorf(".veilignore must always be hidden")
	}
}

func TestMatcher_ActiveConfigHidden(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "rules.txt")
	writeFile(t, cfg, "")

	m, err := NewLiveMatcher(cfg, dir, t.Logf)
	if err != nil {
		t.Fatalf("NewLiveMatcher: %v", err)
	}
	if !m.Match("rules.txt", false) {
		t.Errorf("active config file inside source must be hidden")
	}
}

func TestMatcher_HotReload(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, ".veilignore")
	writeFile(t, cfg, "first.txt\n")

	m, err := NewLiveMatcher(cfg, dir, t.Logf)
	if err != nil {
		t.Fatalf("NewLiveMatcher: %v", err)
	}
	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Stop()

	if !m.Match("first.txt", false) {
		t.Fatalf("initial pattern not applied")
	}
	if m.Match("second.txt", false) {
		t.Fatalf("second.txt should be visible initially")
	}

	writeFile(t, cfg, "second.txt\n")

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if m.Match("second.txt", false) && !m.Match("first.txt", false) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if !m.Match("second.txt", false) {
		t.Errorf("hot reload: second.txt expected to be hidden")
	}
	if m.Match("first.txt", false) {
		t.Errorf("hot reload: first.txt should no longer be hidden")
	}
}

func TestMatcher_DeletionClearsRulesAfterGrace(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, ".veilignore")
	writeFile(t, cfg, "secret.env\n")

	m, err := NewLiveMatcher(cfg, dir, t.Logf)
	if err != nil {
		t.Fatalf("NewLiveMatcher: %v", err)
	}
	// Shorten the grace period to keep tests fast.
	m.clearDelay = 100 * time.Millisecond
	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Stop()

	if !m.Match("secret.env", false) {
		t.Fatalf("initial rule not applied")
	}

	if err := os.Remove(cfg); err != nil {
		t.Fatalf("delete cfg: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !m.Match("secret.env", false) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Errorf("deletion should have cleared rules after the grace window")
}

func TestMatcher_FailClosedOnReloadMissingFile(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, ".veilignore")
	writeFile(t, cfg, "secret.env\n")

	m, err := NewLiveMatcher(cfg, dir, t.Logf)
	if err != nil {
		t.Fatalf("NewLiveMatcher: %v", err)
	}
	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Stop()

	if !m.Match("secret.env", false) {
		t.Fatalf("initial rule not applied")
	}

	// Make the grace window longer than the gap so the deferred clear
	// does not fire during the simulated atomic save.
	m.clearDelay = 2 * time.Second

	// Simulate atomic-save-via-remove: the file briefly disappears
	// before the new one is in place. During that window the previous
	// rules must remain in force.
	if err := os.Remove(cfg); err != nil {
		t.Fatalf("remove cfg: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	if !m.Match("secret.env", false) {
		t.Errorf("reload during transient absence must keep previous rules; secret.env was exposed")
	}

	// Restore with new content and verify the new rule takes over.
	writeFile(t, cfg, "other.env\n")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if m.Match("other.env", false) && !m.Match("secret.env", false) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !m.Match("other.env", false) {
		t.Errorf("new rule did not take effect after recovery")
	}
}

func TestMatcher_CRLFIgnoreFile(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, ".veilignore")
	writeFile(t, cfg, "secret.env\r\nsecrets/\r\n")

	m, err := NewLiveMatcher(cfg, dir, t.Logf)
	if err != nil {
		t.Fatalf("NewLiveMatcher: %v", err)
	}
	if !m.Match("secret.env", false) {
		t.Errorf("CRLF-terminated rule must still match secret.env")
	}
	if !m.Match("secrets/key.pem", false) {
		t.Errorf("CRLF-terminated directory rule must match descendants")
	}
}

func TestMatcher_CaseInsensitive(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, ".veilignore")
	writeFile(t, cfg, "Secret.ENV\nVAULT/\n")

	m, err := NewLiveMatcherWithOptions(cfg, dir, Options{CaseInsensitive: true, Logf: t.Logf})
	if err != nil {
		t.Fatalf("NewLiveMatcher: %v", err)
	}

	cases := []struct {
		path  string
		isDir bool
		want  bool
	}{
		{"Secret.ENV", false, true},
		{"secret.env", false, true},
		{"SECRET.ENV", false, true},
		{"safe.txt", false, false},
		{"vault", true, true},
		{"Vault", true, true},
		{"VAULT/inside.bin", false, true},
		{".veilignore", false, true},
		{".VeilIgnore", false, true}, // active config name compares case-insensitively
	}
	for _, tc := range cases {
		got := m.Match(tc.path, tc.isDir)
		if got != tc.want {
			t.Errorf("case-insensitive Match(%q, isDir=%v) = %v, want %v", tc.path, tc.isDir, got, tc.want)
		}
	}
}

func TestMatcher_CaseSensitiveStaysExact(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, ".veilignore")
	writeFile(t, cfg, "Secret.env\n")

	m, err := NewLiveMatcher(cfg, dir, t.Logf)
	if err != nil {
		t.Fatalf("NewLiveMatcher: %v", err)
	}

	if !m.Match("Secret.env", false) {
		t.Errorf("exact case must match")
	}
	if m.Match("secret.env", false) {
		t.Errorf("differing case must NOT match in case-sensitive mode")
	}
}

func TestMatcher_InitialLoadFailsClosedOnUnreadable(t *testing.T) {
	// --config pointing at a directory (a common misconfiguration)
	// must refuse to construct rather than silently letting the mount
	// come up with an empty rule set.
	dir := t.TempDir()
	subdir := filepath.Join(dir, "not-a-file")
	if err := os.Mkdir(subdir, 0o755); err != nil {
		t.Fatalf("mkdir subdir: %v", err)
	}
	if _, err := NewLiveMatcher(subdir, dir, t.Logf); err == nil {
		t.Errorf("expected initial load to fail when configPath is a directory")
	}
}

func TestMatcher_HotReloadFromMissing(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, ".veilignore")

	m, err := NewLiveMatcher(cfg, dir, t.Logf)
	if err != nil {
		t.Fatalf("NewLiveMatcher: %v", err)
	}
	if err := m.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Stop()

	if m.Match("later.txt", false) {
		t.Fatalf("expected no pattern matched before file exists")
	}

	writeFile(t, cfg, "later.txt\n")

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if m.Match("later.txt", false) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if !m.Match("later.txt", false) {
		t.Errorf("hot reload: later.txt should become hidden after file creation")
	}
}
