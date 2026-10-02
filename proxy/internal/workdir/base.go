package workdir

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/prodeko/prodeko-org/proxy/internal/fence"
)

// The shared read-only view of the origin default branch. Reads that belong to
// no change serve from here, so a conversation that only looks at the site
// never opens a change. One view for everyone: it holds no edits, so there is
// nothing per-person about it.

// baseDirName keeps the view outside every user's directory: userPattern
// refuses a leading dot, so no username can collide with it.
const baseDirName = ".base"

// ErrBaseView answers anything write-shaped asked of the view.
var ErrBaseView = errors.New("workdir: the base view is read-only; open a change by editing a file")

// IsBase reports whether this Change is the shared view. The view is the one
// Change with no branch, and having no branch is exactly why nothing can be
// committed or pushed from it.
func (c *Change) IsBase() bool { return c.Branch == "" }

// Base is the shared view, created on first use and reattached afterwards.
func (m *Manager) Base() (*Change, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.base != nil {
		return m.base, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), GitTimeout)
	defer cancel()
	if err := m.checkClone(ctx); err != nil {
		return nil, err
	}
	base, baseBranch, err := m.baseRef(ctx)
	if err != nil {
		return nil, err
	}

	dir := filepath.Join(m.cfg.StateDir, "wt", baseDirName)
	if !isWorktree(dir) {
		// A worktree whose directory was removed leaves a registration behind,
		// and git refuses to check the same path out again until it is pruned.
		if _, err := m.git(ctx, m.cfg.RepoPath, "worktree", "prune"); err != nil {
			m.log.Warn("workdir: pruning worktrees", "err", err)
		}
		if _, err := m.git(ctx, m.cfg.RepoPath, "fetch", "--quiet", "origin"); err != nil {
			m.log.Warn("workdir: fetching origin", "err", err)
		}
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			return nil, fmt.Errorf("workdir: making the base view directory: %w", err)
		}
		// Detached, because a branch here would be a branch somebody could
		// commit to, and there is nothing to commit from a view of the site as
		// it is published.
		if _, err := m.git(ctx, m.cfg.RepoPath, "worktree", "add", "--quiet", "--detach", dir, base); err != nil {
			return nil, fmt.Errorf("workdir: opening the base view: %w", err)
		}
	}

	f, err := fence.NewFence(dir)
	if err != nil {
		return nil, fmt.Errorf("workdir: fencing %s: %w", dir, err)
	}
	m.base = &Change{
		User: baseDirName, Slug: "base", Branch: "", Dir: dir,
		fence: f, mgr: m, base: base, baseBranch: baseBranch,
	}
	m.log.Info("workdir: base view opened", "dir", dir, "base", base)
	return m.base, nil
}

// RefreshBase brings the view to origin's current default branch. A hard
// reset, because the view holds no edits worth keeping by definition.
func (m *Manager) RefreshBase(ctx context.Context) error {
	b, err := m.Base()
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, GitTimeout)
	defer cancel()
	// A fetch that failed leaves the view on the commit it already had, which
	// is a stale answer rather than no answer; the reset still runs.
	if _, err := m.git(ctx, m.cfg.RepoPath, "fetch", "--quiet", "origin"); err != nil {
		m.log.Warn("workdir: fetching origin for the base view", "err", err)
	}
	if _, err := m.git(ctx, b.Dir, "reset", "--hard", "--quiet", b.base); err != nil {
		return fmt.Errorf("workdir: refreshing the base view: %w", err)
	}
	return nil
}
