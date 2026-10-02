package toolset

import (
	"errors"
	"fmt"
	"strings"

	"github.com/prodeko/prodeko-org/proxy/internal/fence"
	"github.com/prodeko/prodeko-org/proxy/internal/workdir"
)

// The guardrail on submit: the edit loop, enforced.
//
// A media person is asking for a change, not reviewing one. The loop the skill
// teaches — edit, build, look, submit — is what keeps a change that does not
// build or a template nobody looked at out of a maintainer's pull request list,
// and a loop that is only taught is a loop a busy session skips.
//
// The facts come from the change's own stamps on the state volume. The decision
// is here rather than in workdir because a refusal has to name the tool to run,
// and tool names belong to this layer.

// ErrNotReady is a submit the loop has not finished. It is one error carrying
// every missing step, so a model learns all of them in one turn instead of
// discovering them one refusal at a time.
var ErrNotReady = errors.New("submit: this change is not ready")

// gate reports what is missing before this change may be submitted. files is
// what the change touches, against its base branch and in the worktree both,
// and description is the submit argument that becomes the pull request body.
func gate(st workdir.Stamps, files []string, description string) error {
	var missing []string

	// Equal times cannot be ordered, so they read as "the edit came last": a
	// build has to be later than the edit it proves, not simultaneous with it.
	if !st.BuiltAt.After(st.EditedAt) {
		missing = append(missing, fmt.Sprintf(
			"Run %s: nothing has built cleanly since the last edit, so nobody knows whether this change compiles.", ToolBuild))
	}

	// A template renders on every page that uses it, in both languages, and the
	// pages it breaks are the ones nobody was looking at. A screenshot is the
	// cheapest thing that makes the editor look at one of them.
	if templates := layoutFiles(files); len(templates) > 0 {
		changed := strings.Join(templates, ", ")
		rendered := "a page it renders"
		if len(templates) > 1 {
			rendered = "a page they render"
		}
		if !st.ShotAt.After(st.EditedAt) {
			missing = append(missing, fmt.Sprintf(
				"Run %s on %s: %s changed and nothing has been looked at since the last edit.",
				ToolScreenshot, rendered, changed))
		}
		if strings.TrimSpace(description) == "" {
			missing = append(missing,
				"Pass a description of what looks different now: a template changed, and that sentence is what a maintainer reads before the diff.")
		}
	}

	switch len(missing) {
	case 0:
		return nil
	case 1:
		return fmt.Errorf("%w. %s %s again when it is done", ErrNotReady, missing[0], ToolSubmit)
	default:
		var b strings.Builder
		fmt.Fprintf(&b, ". %d things first, then %s again:", len(missing), ToolSubmit)
		for _, m := range missing {
			fmt.Fprintf(&b, "\n  - %s", m)
		}
		return fmt.Errorf("%w%s", ErrNotReady, b.String())
	}
}

// layoutFiles is the templates a change touches. The gate is stricter about
// them than about content because content shows itself in the diff, while a
// template shows itself only where it renders.
func layoutFiles(files []string) []string {
	var out []string
	for _, f := range files {
		if strings.HasPrefix(f, fence.LayoutsRoot) {
			out = append(out, f)
		}
	}
	return out
}
