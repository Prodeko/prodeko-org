package workdir

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/prodeko/prodeko-org/proxy/internal/fence"
)

// File operations on a change. Each one asks the fence first and opens only
// the path the fence resolved, so a symlink planted inside the worktree cannot
// widen what is reachable between the check and the open.

var (
	ErrNotFound    = errors.New("workdir: no such file")
	ErrNoMatch     = errors.New("workdir: the old text does not appear in the file")
	ErrManyMatches = errors.New("workdir: the old text appears more than once; include enough context to make it unique")
	ErrBadRange    = errors.New("workdir: not a usable line range")
)

// defaultSearchResults caps a search the caller put no cap on. The tool schema
// states the same number.
const defaultSearchResults = 100

// Match is one search hit.
type Match struct {
	Path string
	Line int    // 1-based
	Text string // the matching line, trimmed of its newline
}

// ListFiles returns every allowlisted path in the change, relative to the
// repository root and sorted. glob is optional and filters the result with
// path.Match semantics against the whole relative path.
func (c *Change) ListFiles(glob string) ([]string, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var out []string
	err := c.walk(func(rel string, _ fs.DirEntry) error {
		if globMatch(glob, rel) {
			out = append(out, rel)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// ReadFile returns the contents of rel. start and end are 1-based inclusive
// line numbers; zero means unbounded, so (0, 0) is the whole file. A range is
// how main.css gets read without spending fifteen thousand tokens on it.
func (c *Change) ReadFile(rel string, start, end int) (string, error) {
	if start < 0 || end < 0 {
		return "", fmt.Errorf("%w: line numbers are 1-based and positive", ErrBadRange)
	}
	if start > 0 && end > 0 && end < start {
		return "", fmt.Errorf("%w: %d..%d ends before it begins", ErrBadRange, start, end)
	}
	c.mu.RLock()
	defer c.mu.RUnlock()

	abs, err := c.fence.ResolveRead(rel)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(abs)
	if errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("%w: %s", ErrNotFound, rel)
	}
	if err != nil {
		return "", fmt.Errorf("workdir: reading %s: %w", rel, err)
	}
	if start == 0 && end == 0 {
		return string(data), nil
	}

	lines := strings.SplitAfter(string(data), "\n")
	// SplitAfter leaves an empty final element for a file ending in a newline;
	// it is not a line and must not be counted as one.
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	from := start
	if from == 0 {
		from = 1
	}
	if from > len(lines) {
		return "", fmt.Errorf("%w: the file has %d lines and the range starts at %d", ErrBadRange, len(lines), from)
	}
	to := end
	if to == 0 || to > len(lines) {
		to = len(lines)
	}
	return strings.Join(lines[from-1:to], ""), nil
}

// Search returns the lines matching a Go regular expression across the
// allowlisted tree, optionally narrowed by glob, capped at max hits.
func (c *Change) Search(pattern, glob string, max int) ([]Match, error) {
	if strings.TrimSpace(pattern) == "" {
		return nil, errors.New("workdir: a search needs a pattern")
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("workdir: %q is not a usable regular expression: %w", pattern, err)
	}
	if max <= 0 {
		max = defaultSearchResults
	}
	c.mu.RLock()
	defer c.mu.RUnlock()

	var out []Match
	err = c.walk(func(rel string, _ fs.DirEntry) error {
		if len(out) >= max {
			return errStopWalk
		}
		if !globMatch(glob, rel) {
			return nil
		}
		abs, err := c.fence.ResolveRead(rel)
		if err != nil {
			// A file the fence will not open is not a search result; the walk
			// covers the whole allowlisted tree and an oversized stylesheet is
			// no reason to fail the query.
			return nil
		}
		data, err := os.ReadFile(abs)
		if err != nil || bytes.IndexByte(data, 0) >= 0 {
			return nil
		}
		for i, line := range strings.Split(string(data), "\n") {
			if len(out) >= max {
				return errStopWalk
			}
			if re.MatchString(line) {
				out = append(out, Match{Path: rel, Line: i + 1, Text: strings.TrimRight(line, "\r")})
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// WriteFile replaces rel with content, creating it and any missing parent
// directory inside the allowlist.
func (c *Change) WriteFile(rel string, content []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.write(rel, content)
}

// write is WriteFile without the lock, for callers that already hold it.
func (c *Change) write(rel string, content []byte) error {
	abs, err := c.fence.ResolveWrite(rel, int64(len(content)))
	if err != nil {
		return err
	}
	// The path says whether a template may be written at all; this says whether
	// this template and these bytes may be, which is the asset pipeline rule.
	if err := c.fence.CheckTemplate(rel, content); err != nil {
		return err
	}
	if err := c.roomForOneMore(rel, int64(len(content))); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return fmt.Errorf("workdir: making the directory for %s: %w", rel, err)
	}
	// The edit stamp is bumped before the bytes land, never after: submit reads
	// it to insist on a build, and a stamp that cannot be written has to stop
	// the write rather than leave an edit nothing knows about. A stamp slightly
	// older than the write it records still orders correctly against the build
	// that preceded it, which is the only comparison anybody makes of it.
	if err := c.markEdit(c.mgr.cfg.Now()); err != nil {
		return err
	}
	if err := os.WriteFile(abs, content, 0o644); err != nil {
		return fmt.Errorf("workdir: writing %s: %w", rel, err)
	}
	return nil
}

// roomForOneMore refuses a write that would take the change past the
// per-change limits: at most MaxChangeFiles files and MaxTextBytes of text
// across all of them. Both are enforced again at submit against what git sees;
// this makes the refusal land on the write that caused it, and keeps fifty
// maximum-sized writes from filling the state volume before anything is
// submitted at all.
func (c *Change) roomForOneMore(rel string, size int64) error {
	touched, err := c.Touched()
	if err != nil {
		return err
	}
	known := false
	total := size
	for _, p := range touched {
		if p == rel {
			known = true
			continue
		}
		// Lstat, not Stat: a symlink counts as itself, never as its target.
		if info, err := os.Lstat(filepath.Join(c.Dir, filepath.FromSlash(p))); err == nil {
			total += info.Size()
		}
	}
	if !known && len(touched) >= fence.MaxChangeFiles {
		return fmt.Errorf("%w: the change already touches %d files, at most %d; submit it and start another",
			fence.ErrTooManyFiles, len(touched), fence.MaxChangeFiles)
	}
	if total > fence.MaxTextBytes {
		return fmt.Errorf("%w: the change would hold %d bytes, at most %d in one change; submit it and start another",
			fence.ErrTooLarge, total, fence.MaxTextBytes)
	}
	return nil
}

// EditFile replaces the one occurrence of old in rel with replacement. It is
// an error for old to appear zero times or more than once: an edit that could
// land in two places is not an edit the caller described.
func (c *Change) EditFile(rel, old, replacement string) error {
	if old == "" {
		return fmt.Errorf("%w: the old text is empty", ErrNoMatch)
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	abs, err := c.fence.ResolveRead(rel)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(abs)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: %s", ErrNotFound, rel)
	}
	if err != nil {
		return fmt.Errorf("workdir: reading %s: %w", rel, err)
	}
	switch n := strings.Count(string(data), old); n {
	case 1:
	case 0:
		return fmt.Errorf("%w: %s", ErrNoMatch, rel)
	default:
		return fmt.Errorf("%w: %d occurrences in %s", ErrManyMatches, n, rel)
	}
	return c.write(rel, []byte(strings.Replace(string(data), old, replacement, 1)))
}

// Touched lists the paths this change has modified against its base branch:
// what is committed on the branch and what is still only in the worktree.
func (c *Change) Touched() ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), GitTimeout)
	defer cancel()

	seen := make(map[string]bool)
	committed, err := c.mgr.git(ctx, c.Dir, "diff", "--name-only", c.base+"...HEAD")
	if err != nil {
		return nil, fmt.Errorf("workdir: listing the change: %w", err)
	}
	for _, p := range strings.Split(committed, "\n") {
		if p = strings.TrimSpace(p); p != "" {
			seen[p] = true
		}
	}
	dirty, err := c.dirtyPaths(ctx)
	if err != nil {
		return nil, err
	}
	for _, p := range dirty {
		seen[p] = true
	}

	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}

// errStopWalk ends a walk early once a caller has all it asked for. It never
// leaves this package.
var errStopWalk = errors.New("workdir: walk complete")

// walk visits every allowlisted file in the worktree. It descends only the
// allowlist's own roots, so the build output, node_modules and the .git
// directory are never read at all rather than being read and discarded.
func (c *Change) walk(visit func(rel string, d fs.DirEntry) error) error {
	root := c.fence.Root()
	for _, rule := range fence.Rules() {
		base := filepath.Join(root, filepath.FromSlash(strings.TrimSuffix(rule.Prefix, "/")))
		err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				return err
			}
			if d.IsDir() {
				if strings.HasPrefix(d.Name(), ".") {
					return fs.SkipDir
				}
				return nil
			}
			if !d.Type().IsRegular() {
				return nil
			}
			rel, relErr := filepath.Rel(root, p)
			if relErr != nil {
				return nil
			}
			rel = filepath.ToSlash(rel)
			if !c.fence.Visible(rel) {
				return nil
			}
			return visit(rel, d)
		})
		if errors.Is(err, errStopWalk) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("workdir: listing %s: %w", rule.Prefix, err)
		}
	}
	return nil
}

// globMatch filters a repository-relative path. It is path.Match over the
// whole path, with two additions the tool schemas promise: ** matches any run
// of segments, so site/content/fi/** reaches a whole subtree, and a pattern
// with no slash in it is also tried against the base name, so *.css means what
// it looks like it means.
func globMatch(pattern, rel string) bool {
	if pattern == "" {
		return true
	}
	if matchSegments(strings.Split(pattern, "/"), strings.Split(rel, "/")) {
		return true
	}
	if !strings.Contains(pattern, "/") {
		ok, err := path.Match(pattern, path.Base(rel))
		return err == nil && ok
	}
	return false
}

func matchSegments(pattern, segments []string) bool {
	for len(pattern) > 0 {
		if pattern[0] == "**" {
			if len(pattern) == 1 {
				return true
			}
			for i := 0; i <= len(segments); i++ {
				if matchSegments(pattern[1:], segments[i:]) {
					return true
				}
			}
			return false
		}
		if len(segments) == 0 {
			return false
		}
		ok, err := path.Match(pattern[0], segments[0])
		if err != nil || !ok {
			return false
		}
		pattern, segments = pattern[1:], segments[1:]
	}
	return len(segments) == 0
}
