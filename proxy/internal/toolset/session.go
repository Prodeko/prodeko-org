package toolset

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/prodeko/prodeko-org/proxy/internal/mcpserver"
	"github.com/prodeko/prodeko-org/proxy/internal/workdir"
)

// A session is one working stretch: the change being edited, if any, and when
// the person was last heard from. It expires by going idle, which is the only
// conversation boundary a stateless transport lets this server see.
//
// One person's simultaneous conversations share a session. The server cannot
// tell them apart without MCP session ids, and binding by person is the shape
// a session key would slot into.

// SessionIdle is how long a session survives silence. Longer than a coffee
// break, shorter than "yesterday": a person answering review feedback the next
// morning starts fresh and picks the old change up deliberately.
const SessionIdle = 45 * time.Minute

// session is what one person's conversation is working on. Every field is read
// and written under the tool set's mutex: two calls from the same person can
// be in flight at once, and the binding is the thing they would disagree
// about.
type session struct {
	change      *workdir.Change // nil until the first write or resume_change
	lastUsed    time.Time
	notePending bool // the open-changes note has not been delivered
	baseFresh   bool // the base view was brought up to date for this session

	// opening is held while this session's change is being made. Opening one
	// takes git several seconds, which is long enough for the next tool call of
	// the same turn to arrive, see no change and open a second one; the loser of
	// that race would take half the person's edits with it.
	opening sync.Mutex
}

// sessionFor returns the caller's live session, starting one when none is.
// The second return says a session was started, which is what the note and the
// base refresh key on.
func (t *Toolset) sessionFor(user string) (*session, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := t.now()
	if s, ok := t.sessions[user]; ok && now.Sub(s.lastUsed) <= SessionIdle {
		s.lastUsed = now
		return s, false
	}
	s := &session{lastUsed: now, notePending: true}
	t.sessions[user] = s
	return s, true
}

// reading is the tree a read serves from: the change this conversation is
// working on, or the shared view of the published site when it is working on
// nothing yet. Looking at the site therefore costs no change at all.
func (t *Toolset) reading(ctx context.Context, id mcpserver.Identity) (*workdir.Change, error) {
	user, err := userOf(id)
	if err != nil {
		return nil, err
	}
	s, _ := t.sessionFor(user)

	t.mu.Lock()
	c := s.change
	refresh := c == nil && !s.baseFresh
	if refresh {
		s.baseFresh = true
	}
	t.mu.Unlock()

	if c != nil {
		return c, nil
	}
	if refresh {
		// Once a session, because origin moves when other people's work is
		// merged and a conversation starting today must not read yesterday's
		// site. A refresh that failed leaves a stale view, which is still an
		// answer; the read goes on.
		if err := t.mgr.RefreshBase(ctx); err != nil {
			t.log.Warn("toolset: refreshing the base view", "err", err)
		}
	}
	return t.mgr.Base()
}

// writing is the tree an edit lands in: the change this conversation is
// working on, or a fresh one named after what was asked for, which becomes the
// conversation's change.
func (t *Toolset) writing(id mcpserver.Identity, hint string) (*workdir.Change, error) {
	user, err := userOf(id)
	if err != nil {
		return nil, err
	}
	return t.writingUser(user, hint)
}

// writingUser is writing for a caller that already holds the namespaced
// username: the upload handler, whose token carries it instead of a bearer
// identity.
func (t *Toolset) writingUser(user, hint string) (*workdir.Change, error) {
	s, _ := t.sessionFor(user)

	// One at a time, so the second write of a turn waits here and then finds
	// what the first one opened instead of opening another.
	s.opening.Lock()
	defer s.opening.Unlock()

	if c := t.boundChange(user); c != nil {
		return c, nil
	}
	// Fresh rather than by name: the name comes from the file, so the name a
	// conversation picks today is the name the last one picked, and continuing
	// that work is something to be asked for by resume_change.
	c, err := t.mgr.Fresh(user, hint, t.now())
	if err != nil {
		return nil, err
	}
	t.bind(user, c)
	return c, nil
}

// boundChange is the change this conversation is working on, or nil when it
// has not opened one. A tool that needs a change and is given no name answers
// with that nil rather than opening something nobody asked for.
func (t *Toolset) boundChange(user string) *workdir.Change {
	s, _ := t.sessionFor(user)
	t.mu.Lock()
	defer t.mu.Unlock()
	return s.change
}

// bind sets the session's change. The first write and resume_change are the
// only callers: nothing else decides what a conversation is working on. It
// starts the session itself when none is live, because a resume can be the
// very first call a process sees, and a binding that needs somebody else to
// have opened the session would leave the "Resumed" answer standing over
// edits that land elsewhere.
func (t *Toolset) bind(user string, c *workdir.Change) {
	s, _ := t.sessionFor(user)
	t.mu.Lock()
	defer t.mu.Unlock()
	s.change = c
}

// unbind drops the change a conversation was working on, so the next edit
// starts fresh instead of landing in a worktree that is gone.
func (t *Toolset) unbind(user string, c *workdir.Change) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if s, ok := t.sessions[user]; ok && sameChange(s.change, c) {
		s.change = nil
	}
}

// released is what the manager calls when a change has been archived. The
// sweep runs wherever GitHub is consulted — a listing, the once-a-session note
// — so it can end the very change the conversation is editing, and a binding
// that outlived its worktree answers every later call with a path nobody
// mentioned. Dropping it here means the next edit starts a change instead.
func (t *Toolset) released(c *workdir.Change) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, s := range t.sessions {
		if sameChange(s.change, c) {
			s.change = nil
		}
	}
}

// sameChange is identity by branch rather than by pointer: a change looked up
// twice is the same change, and the branch is what makes it one.
func sameChange(a, b *workdir.Change) bool {
	return a != nil && b != nil && a.Branch != "" && a.Branch == b.Branch
}

// note is the once-a-session mention of what is already open. It rides on the
// first answer rather than being a message of its own, because a stateless
// transport gives this server no way to speak first: the one moment the model
// can be told about yesterday's half-finished work is the moment it says
// anything at all.
//
// It is spent whether or not it comes to anything, so an errand that turns out
// to have nothing to report does not hand the note on to the next call.
func (t *Toolset) note(ctx context.Context, id mcpserver.Identity) string {
	user, err := userOf(id)
	if err != nil {
		return ""
	}

	t.mu.Lock()
	s, ok := t.sessions[user]
	pending := ok && s.notePending
	var bound string
	if pending {
		s.notePending = false
		if s.change != nil {
			bound = s.change.Slug
		}
	}
	t.mu.Unlock()
	if !pending {
		return ""
	}

	// The listing every open change is read from, so the note and
	// list_my_changes cannot disagree about what is open — and so a change
	// merged since the last conversation is swept away rather than offered.
	infos, err := t.mgr.List(ctx, user)
	if err != nil {
		t.log.Warn("toolset: listing open changes for the note", "user", user, "err", err)
		return ""
	}
	return renderNote(infos, bound)
}

// spendNote marks the note delivered without delivering it. list_my_changes is
// the one tool whose own answer is the note's contents in full; appending it
// there would print the same list twice.
func (t *Toolset) spendNote(user string) {
	s, _ := t.sessionFor(user)
	t.mu.Lock()
	defer t.mu.Unlock()
	s.notePending = false
}

// noted appends the session's note to a tool's answer. Every tool is wrapped,
// because which one a conversation opens with is the client's business: the
// note belongs to the first call, not to a particular errand.
func (t *Toolset) noted(fn func(context.Context, mcpserver.Identity, json.RawMessage) (mcpserver.Result, error)) func(context.Context, mcpserver.Identity, json.RawMessage) (mcpserver.Result, error) {
	return func(ctx context.Context, id mcpserver.Identity, args json.RawMessage) (mcpserver.Result, error) {
		res, err := fn(ctx, id, args)
		if err != nil {
			// A refusal is confusing enough without a postscript about
			// something else, and the note keeps until the next answer.
			return res, err
		}
		res.Text += t.note(ctx, id)
		return res, nil
	}
}

// noChange is the answer to a tool that needs the conversation's change when
// there is none. A conversation starts on a clean checkout, so having nothing
// open is its ordinary state and not a failure worth an error.
func noChange(errand string) string {
	return "No change is open in this conversation, so there is nothing to " + errand +
		". An edit opens one; " + ToolResumeChange + " continues an existing one."
}
