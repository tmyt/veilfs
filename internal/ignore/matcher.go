// Package ignore implements a hot-reloadable .veilignore matcher.
//
// A LiveMatcher loads gitignore-style patterns from a single configuration
// file and atomically swaps the compiled rules whenever that file changes
// on disk. Match decisions are made against paths relative to a known
// source root, with .veilignore files (and the active config file, if it
// lives inside the source tree) always reported as hidden so that the
// veil itself is not exposed through the FUSE mount.
//
// Once a directory is hidden, every descendant is also reported as hidden:
// negation patterns cannot resurface a child whose parent is excluded.
// This matches Git's documented behaviour and the practical reality that
// the FUSE layer returns ENOENT for the parent, making the descendant
// unreachable through the mount regardless of what the matcher claims.
package ignore

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
	gitignore "github.com/sabhiram/go-gitignore"
)

// IgnoreFileName is the conventional name of the veilfs ignore file at the
// root of the source tree.
const IgnoreFileName = ".veilignore"

// LiveMatcher applies a single ignore configuration file and reloads it
// whenever the file changes.
type LiveMatcher struct {
	sourceRoot      string
	configPath      string
	selfRel         string // configPath relative to sourceRoot, "" if outside
	caseInsensitive bool
	current         atomic.Pointer[gitignore.GitIgnore]
	logf            func(format string, args ...any)

	watcher *fsnotify.Watcher
	done    chan struct{}

	clearMu      sync.Mutex
	clearTimer   *time.Timer
	clearDelay   time.Duration
}

// clearGrace is the window we allow editor atomic-save patterns to
// repopulate the ignore file before we treat its absence as a
// deliberate deletion and unhide everything.
const clearGrace = 500 * time.Millisecond

// Options tunes how a LiveMatcher evaluates patterns. The zero value is
// case-sensitive matching with a no-op logger.
type Options struct {
	// CaseInsensitive folds both patterns and incoming paths to lower
	// case before matching, which is required when the backing
	// filesystem is itself case-insensitive (APFS-default, HFS+, NTFS,
	// exFAT, …). The fold is performed via strings.ToLower and is
	// therefore accurate for ASCII; non-ASCII case folding is best
	// effort and may diverge from the filesystem in edge cases (e.g.
	// Turkish dotless i, German ß).
	CaseInsensitive bool

	// Logf receives diagnostic messages from the matcher. May be nil.
	Logf func(format string, args ...any)
}

// NewLiveMatcher constructs a matcher backed by the file at configPath.
// configPath need not exist at construction time: the matcher starts with
// an empty pattern set and picks up the file once it is created.
// sourceRoot and configPath must both be absolute paths.
//
// Provided for backward compatibility; new code should prefer
// NewLiveMatcherWithOptions.
func NewLiveMatcher(configPath, sourceRoot string, logf func(string, ...any)) (*LiveMatcher, error) {
	return NewLiveMatcherWithOptions(configPath, sourceRoot, Options{Logf: logf})
}

// NewLiveMatcherWithOptions is like NewLiveMatcher but takes an Options
// struct so callers can request case-insensitive matching or supply a
// logger.
func NewLiveMatcherWithOptions(configPath, sourceRoot string, opts Options) (*LiveMatcher, error) {
	if !filepath.IsAbs(configPath) {
		return nil, errors.New("ignore: configPath must be absolute")
	}
	if !filepath.IsAbs(sourceRoot) {
		return nil, errors.New("ignore: sourceRoot must be absolute")
	}
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	m := &LiveMatcher{
		sourceRoot:      filepath.Clean(sourceRoot),
		configPath:      filepath.Clean(configPath),
		caseInsensitive: opts.CaseInsensitive,
		logf:            logf,
	}
	if rel, err := filepath.Rel(m.sourceRoot, m.configPath); err == nil &&
		rel != "." && !strings.HasPrefix(rel, "..") &&
		!strings.HasPrefix(rel, string(filepath.Separator)) {
		m.selfRel = filepath.ToSlash(rel)
	}
	if err := m.initialLoad(); err != nil {
		return nil, err
	}
	return m, nil
}

// initialLoad performs the very first read of the ignore file. Unlike
// subsequent reloads, a hard read error here is propagated to the
// caller so callers can refuse to mount. The only acceptable
// pre-existence state is "file is absent" — that is treated as an
// empty rule set so the mount can come up and later observe a created
// file via fsnotify.
func (m *LiveMatcher) initialLoad() error {
	data, err := os.ReadFile(m.configPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			m.current.Store(gitignore.CompileIgnoreLines())
			return nil
		}
		return fmt.Errorf("ignore: initial read of %s: %w", m.configPath, err)
	}
	m.applyLines(strings.Split(string(data), "\n"))
	return nil
}

// load reads the ignore file and replaces the active matcher on success.
//
// On read failure the previous matcher is retained — fail-closed during a
// reload preserves the secrecy guarantee through editor atomic-save
// patterns and transient I/O errors. The only time an unreadable file
// yields an empty matcher is the very first call, when no rules have
// ever been loaded; otherwise the user would see no enforcement until
// the file reappeared.
// load is called from the watcher goroutine on each ignore-file change.
// Transient read failures preserve the previous matcher so editor
// atomic-save patterns and permission blips do not silently disable
// enforcement. A persistent ENOENT (the user deliberately deletes the
// file) is treated specially: we schedule a delayed clear so the
// quick gap between rename-out and rename-in during atomic save is
// tolerated, but a real deletion does eventually take effect.
func (m *LiveMatcher) load() {
	data, err := os.ReadFile(m.configPath)
	if err != nil {
		if m.current.Load() == nil {
			m.current.Store(gitignore.CompileIgnoreLines())
		}
		if errors.Is(err, fs.ErrNotExist) {
			m.scheduleClearAfterGrace()
		} else {
			m.logf("veilfs: read %s: %v (keeping previous rules)", m.configPath, err)
		}
		return
	}
	m.cancelClear()
	m.applyLines(strings.Split(string(data), "\n"))
}

func (m *LiveMatcher) scheduleClearAfterGrace() {
	m.clearMu.Lock()
	defer m.clearMu.Unlock()
	if m.clearTimer != nil {
		return
	}
	delay := m.clearDelay
	if delay == 0 {
		delay = clearGrace
	}
	m.clearTimer = time.AfterFunc(delay, func() {
		m.clearMu.Lock()
		m.clearTimer = nil
		m.clearMu.Unlock()
		if _, err := os.Stat(m.configPath); errors.Is(err, fs.ErrNotExist) {
			m.current.Store(gitignore.CompileIgnoreLines())
			m.logf("veilfs: %s deleted; ignore rules cleared", m.configPath)
		}
	})
}

func (m *LiveMatcher) cancelClear() {
	m.clearMu.Lock()
	defer m.clearMu.Unlock()
	if m.clearTimer != nil {
		m.clearTimer.Stop()
		m.clearTimer = nil
	}
}

func (m *LiveMatcher) applyLines(lines []string) {
	for i, line := range lines {
		// Tolerate CRLF-saved configs: a trailing \r left from
		// "secret.env\r" would silently break matching otherwise.
		line = strings.TrimRight(line, "\r")
		if m.caseInsensitive {
			line = strings.ToLower(line)
		}
		lines[i] = line
	}
	m.current.Store(gitignore.CompileIgnoreLines(lines...))
}

// Match reports whether the given source-relative path must be hidden.
// rel uses forward slashes and contains no leading slash. isDir indicates
// whether the entry refers to a directory.
func (m *LiveMatcher) Match(rel string, isDir bool) bool {
	rel = strings.TrimPrefix(rel, "/")
	if rel == "" {
		return false
	}
	if m.caseInsensitive {
		if strings.EqualFold(filepath.Base(rel), IgnoreFileName) {
			return true
		}
		if m.selfRel != "" && strings.EqualFold(rel, m.selfRel) {
			return true
		}
		rel = strings.ToLower(rel)
	} else {
		if filepath.Base(rel) == IgnoreFileName {
			return true
		}
		if m.selfRel != "" && rel == m.selfRel {
			return true
		}
	}
	gi := m.current.Load()
	if gi == nil {
		return false
	}
	if gi.MatchesPath(rel) {
		return true
	}
	if isDir && gi.MatchesPath(rel+"/") {
		return true
	}
	parts := strings.Split(rel, "/")
	for i := 1; i < len(parts); i++ {
		anc := strings.Join(parts[:i], "/")
		if gi.MatchesPath(anc) || gi.MatchesPath(anc+"/") {
			return true
		}
	}
	return false
}

// MatchAny returns true if either the file or directory interpretation of
// the path matches. Callers should use this when validating an operation
// that might affect either kind of entry (e.g. a rename destination), and
// want to deny on any possible collision with an ignored path.
func (m *LiveMatcher) MatchAny(rel string) bool {
	return m.Match(rel, false) || m.Match(rel, true)
}

// Start synchronously registers a filesystem watch on the directory
// containing the configured ignore file, then launches a background
// goroutine that reloads patterns when the file changes. The parent
// directory (rather than the file itself) is watched so that editor
// atomic-save patterns, which replace the inode, continue to be observed.
//
// Returns an error if the watch cannot be registered. The caller must
// invoke Stop to release resources.
func (m *LiveMatcher) Start() error {
	if m.watcher != nil {
		return errors.New("ignore: matcher already started")
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	if err := w.Add(filepath.Dir(m.configPath)); err != nil {
		w.Close()
		return err
	}
	m.watcher = w
	m.done = make(chan struct{})
	go m.runLoop()
	return nil
}

// Stop tears down the watcher and waits for the background goroutine to
// exit. Safe to call multiple times.
func (m *LiveMatcher) Stop() {
	m.cancelClear()
	if m.watcher == nil {
		return
	}
	m.watcher.Close()
	<-m.done
	m.watcher = nil
	m.done = nil
}

// CaseInsensitive reports whether the matcher folds case when comparing
// patterns and paths. The vfs layer consults this when deciding how to
// compare symlink targets against the source root.
func (m *LiveMatcher) CaseInsensitive() bool {
	return m.caseInsensitive
}

func (m *LiveMatcher) runLoop() {
	defer close(m.done)
	base := filepath.Base(m.configPath)
	const reloadOps = fsnotify.Write | fsnotify.Create | fsnotify.Rename | fsnotify.Remove | fsnotify.Chmod
	for {
		select {
		case ev, ok := <-m.watcher.Events:
			if !ok {
				return
			}
			if filepath.Base(ev.Name) != base {
				continue
			}
			if ev.Op&reloadOps != 0 {
				m.load()
			}
		case err, ok := <-m.watcher.Errors:
			if !ok {
				return
			}
			m.logf("veilfs: watcher: %v", err)
		}
	}
}
