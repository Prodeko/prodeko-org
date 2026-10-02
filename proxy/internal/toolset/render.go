package toolset

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/prodeko/prodeko-org/proxy/internal/lint"
	"github.com/prodeko/prodeko-org/proxy/internal/preview"
	"github.com/prodeko/prodeko-org/proxy/internal/workdir"
)

// What a tool returns is prose the model reads, so it is written for that
// reader: the result first, then what to do next where there is something to
// do. Nothing here interprets hugo's or git's output; a build error and a
// diffstat are quoted exactly as they came.

// maxMatchLine bounds one line of a search result. A stylesheet has no long
// lines, but a data file can carry one, and five hundred of them would fill
// the model's context with something it did not ask for.
const maxMatchLine = 300

func renderFiles(files []string, glob string) string {
	if len(files) == 0 {
		if glob != "" {
			return fmt.Sprintf("No editable file matches %q.", glob)
		}
		return "The editable tree is empty."
	}
	var b strings.Builder
	for _, f := range files {
		b.WriteString(f)
		b.WriteByte('\n')
	}
	fmt.Fprintf(&b, "\n%d files", len(files))
	if glob != "" {
		fmt.Fprintf(&b, " matching %q", glob)
	}
	b.WriteByte('.')
	return b.String()
}

func renderMatches(matches []workdir.Match, pattern string, capped int) string {
	if len(matches) == 0 {
		return fmt.Sprintf("No line matches %s.", pattern)
	}
	var b strings.Builder
	for _, m := range matches {
		text := m.Text
		if len(text) > maxMatchLine {
			text = text[:maxMatchLine] + " ..."
		}
		fmt.Fprintf(&b, "%s:%d: %s\n", m.Path, m.Line, text)
	}
	if len(matches) >= capped {
		fmt.Fprintf(&b, "\nStopped at %d matches. Narrow the pattern or pass a glob.", capped)
	}
	return b.String()
}

func renderBuild(res workdir.Result) string {
	var b strings.Builder
	out := strings.TrimRight(res.Output, "\n")
	if res.OK {
		fmt.Fprintf(&b, "Build OK in %s.", res.Duration.Round(time.Millisecond))
		if out != "" {
			fmt.Fprintf(&b, "\n\n%s", out)
		}
	} else {
		if out == "" {
			out = "(the build failed and said nothing)"
		}
		fmt.Fprintf(&b, "Build failed after %s. The output is verbatim:\n\n%s",
			res.Duration.Round(time.Millisecond), out)
	}
	// The findings come after hugo's words and never instead of them, and they
	// do not change what the build said about itself: the site either built or
	// it did not, and these are things to look at on the way past.
	writeFindings(&b, "The stylesheets", res.CSS,
		"A stylesheet a browser cannot parse is a stylesheet it drops rules out of, quietly.")
	writeFindings(&b, "The built pages", res.HTML,
		"Markup a browser has to guess at is markup that lands differently in different browsers.")
	return b.String()
}

// writeFindings quotes what a check noticed, one line each, exactly as it was
// found. The closing sentence is why the list is worth a turn; without it a
// model reading "Build OK" has every reason to move on.
func writeFindings(b *strings.Builder, what string, findings []lint.Finding, why string) {
	if len(findings) == 0 {
		return
	}
	fmt.Fprintf(b, "\n\n%s, %d thing", what, len(findings))
	if len(findings) > 1 {
		b.WriteByte('s')
	}
	b.WriteString(":\n")
	for _, f := range findings {
		fmt.Fprintf(b, "  %s\n", f)
	}
	if len(findings) >= lint.MaxFindings {
		fmt.Fprintf(b, "  ... stopped at %d; the same mistake in one template shows on every page that uses it.\n", lint.MaxFindings)
	}
	b.WriteString(why)
}

func renderSubmit(res workdir.SubmitResult) string {
	var b strings.Builder
	// A dry run says what it did once, in the note the workdir wrote; a second
	// headline here would be a second, differently worded claim about the same
	// push.
	if res.PRNumber > 0 {
		fmt.Fprintf(&b, "Draft pull request #%d: %s\n", res.PRNumber, res.PRURL)
		if res.PreviewURL != "" {
			fmt.Fprintf(&b, "Preview: %s (ready in about a minute)\n", res.PreviewURL)
		}
	} else if !res.DryRun {
		b.WriteString("Committed and pushed.\n")
	}
	if res.NothingNew {
		b.WriteString("Nothing had changed since the last submit, so no new commit was made.\n")
	}
	fmt.Fprintf(&b, "Branch: %s\n", res.Branch)
	if res.Commit != "" {
		fmt.Fprintf(&b, "Commit: %s\n", res.Commit)
	}
	if len(res.Files) > 0 {
		b.WriteString("Files:\n")
		for _, f := range res.Files {
			fmt.Fprintf(&b, "  %s\n", f)
		}
	}
	if d := strings.TrimRight(res.Diffstat, "\n"); d != "" {
		fmt.Fprintf(&b, "%s\n", d)
	}
	if res.Note != "" {
		fmt.Fprintf(&b, "%s\n", res.Note)
	}
	b.WriteString("\nAsking for another change continues this branch and the same pull request.")
	return b.String()
}

// renderPage introduces one built page and then quotes it. The HTML is the
// answer and is handed over exactly as hugo wrote it, on the far side of a
// blank line so the sentence about it cannot be mistaken for part of it.
func renderPage(p preview.Page, selector, out string) string {
	var b strings.Builder
	if strings.TrimSpace(selector) == "" {
		fmt.Fprintf(&b, "%s, as the last build wrote it, %d bytes:\n\n", p.URL, len(out))
	} else {
		fmt.Fprintf(&b, "%s, the part matching %q, %d bytes:\n\n", p.URL, strings.TrimSpace(selector), len(out))
	}
	if len(out) <= MaxHTMLBytes {
		b.WriteString(out)
		return b.String()
	}
	b.WriteString(truncate(out, MaxHTMLBytes))
	fmt.Fprintf(&b, "\n\n... stopped at %d bytes of %d. Pass a CSS selector to read one part of the page.",
		MaxHTMLBytes, len(out))
	return b.String()
}

// truncate cuts to at most n bytes without splitting a character in half.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.ValidString(s[:n]) {
		n--
	}
	return s[:n]
}

// shot is one page and the picture taken of it.
type shot struct {
	page  preview.Page
	taken preview.Shot
}

// renderShots says what the pictures are, in the order they follow it. The model
// sees images and no filenames, so the prose is the only thing that tells it
// which language it is looking at.
func renderShots(shots []shot, width int, note string) string {
	var b strings.Builder
	for _, s := range shots {
		fmt.Fprintf(&b, "%s at %d px wide: %d by %d pixels.\n", s.page.URL, width, s.taken.Width, s.taken.Height)
		if s.taken.Cut {
			fmt.Fprintf(&b, "  The page is taller than the capture; this is its top %d pixels.\n", s.taken.Height)
		}
	}
	switch {
	case len(shots) > 1:
		fmt.Fprintf(&b, "\n%d languages, paired by translationKey %q. The pictures follow in the order above.\n",
			len(shots), shots[0].page.TranslationKey)
	case len(shots) == 1 && shots[0].page.TranslationKey != "":
		fmt.Fprintf(&b, "\nNo built counterpart carries translationKey %q, so this is %s alone.\n",
			shots[0].page.TranslationKey, shots[0].page.Lang)
	}
	if note != "" {
		fmt.Fprintf(&b, "\n%s\n", note)
	}
	return strings.TrimRight(b.String(), "\n")
}

func renderChanges(infos []workdir.Info) string {
	if len(infos) == 0 {
		return "You have no open changes. The first edit starts one."
	}
	var b strings.Builder
	for i, in := range infos {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%s\n", in.Slug)
		writeChange(&b, in)
	}
	return strings.TrimRight(b.String(), "\n")
}

// writeChange is one change under its name: what it touches, and what has
// become of it on GitHub. list_my_changes and resume_change report the same
// things about a change, because they are the same things to act on.
func writeChange(b *strings.Builder, in workdir.Info) {
	fmt.Fprintf(b, "  branch: %s\n", in.Branch)
	if !in.UpdatedAt.IsZero() {
		fmt.Fprintf(b, "  updated: %s\n", in.UpdatedAt.Format(time.RFC3339))
	}
	if len(in.Files) > 0 {
		fmt.Fprintf(b, "  files: %s\n", strings.Join(in.Files, ", "))
	}
	// A change with no pull request is not necessarily unsubmitted: in a dry
	// run there is never one to find, and saying "not submitted yet" about
	// a change that is committed and pushed would send the editor back to
	// submit a second time. Files with a clean worktree is what "committed"
	// looks like from here.
	switch {
	case in.PRNumber > 0:
		fmt.Fprintf(b, "  pull request: #%d %s\n", in.PRNumber, in.PRURL)
		if in.CIState != "" {
			fmt.Fprintf(b, "  checks: %s\n", in.CIState)
		}
		if in.PreviewURL != "" {
			fmt.Fprintf(b, "  preview: %s\n", in.PreviewURL)
		}
	case in.Dirty || len(in.Files) == 0:
		b.WriteString("  not submitted yet\n")
	default:
		b.WriteString("  committed to the branch, with no pull request\n")
	}
	if in.Dirty && len(in.Files) > 0 {
		b.WriteString("  has edits that were never submitted\n")
	}
}

// renderResume says what the person has just picked up. The state a resumed
// change is in is the first thing to act on: an edit onto a branch whose review
// asked for something is different work from an edit onto one nobody has seen
// yet. A change the listing could not describe is named and no more, rather
// than being given a state it was never read to have.
func renderResume(in workdir.Info, listed bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Resumed %s.\n", in.Slug)
	if listed {
		writeChange(&b, in)
	} else {
		fmt.Fprintf(&b, "  branch: %s\n", in.Branch)
	}
	b.WriteString("\nEdits now land on this change.")
	return b.String()
}

// noteHeading opens the once-a-session note. It says where the changes came
// from, because that is what makes the list worth reading: everything on it is
// work the person may have meant to carry on with.
const noteHeading = "\n\nNote: you have open changes from before this conversation:\n"

// renderNote is that note: one line a change, short enough to ride on the back
// of an answer to something else. Where renderChanges describes changes because
// they were asked about, this interrupts, so it says only what decides whether
// to pick one up — how far it got, and whether anything in it was never shown to
// anybody. skip is the change the conversation is already on; it is on the
// listing but is not news.
func renderNote(infos []workdir.Info, skip string) string {
	var b strings.Builder
	for _, in := range infos {
		if in.Slug == skip {
			continue
		}
		files := fmt.Sprintf("%d files", len(in.Files))
		if len(in.Files) == 1 {
			files = "1 file"
		}
		fmt.Fprintf(&b, "  %s: %s", in.Slug, files)
		// The same three cases writeChange makes, and for the same reason: a dry
		// run has no pull request to find, so a missing number does not mean
		// nothing was pushed. Unsubmitted edits are worth naming only where the
		// change got further than that, which is the one case they are news.
		switch {
		case in.PRNumber > 0:
			fmt.Fprintf(&b, ", pull request #%d", in.PRNumber)
			if in.CIState != "" {
				fmt.Fprintf(&b, ", checks %s", in.CIState)
			}
			if in.Dirty {
				b.WriteString(", with edits made since")
			}
		case in.Dirty || len(in.Files) == 0:
			b.WriteString(", not submitted yet")
		default:
			b.WriteString(", committed with no pull request")
		}
		b.WriteByte('\n')
	}
	if b.Len() == 0 {
		return ""
	}
	return noteHeading + b.String() + ToolResumeChange +
		" continues one of them; otherwise your first edit starts a new change."
}

// renderFeedback is the pull request as the maintainers left it. Bodies are
// verbatim and indented, so where GitHub's words end and this server's begin
// stays visible.
func renderFeedback(slug string, fb workdir.Feedback) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s is pull request #%d (%s), %s.\n", slug, fb.PRNumber, fb.PRURL, fb.State)
	if fb.CIState != "" {
		fmt.Fprintf(&b, "Checks: %s.\n", fb.CIState)
	}
	if fb.PreviewURL != "" && fb.State == "open" {
		fmt.Fprintf(&b, "Preview: %s\n", fb.PreviewURL)
	}
	if len(fb.Reviews) == 0 && len(fb.Comments) == 0 {
		b.WriteString("Nobody has reviewed or commented yet.")
		return b.String()
	}
	for _, r := range fb.Reviews {
		fmt.Fprintf(&b, "\nReview by %s: %s", r.Author, r.State)
		if !r.At.IsZero() {
			fmt.Fprintf(&b, " (%s)", r.At.Format(time.RFC3339))
		}
		b.WriteByte('\n')
		writeQuoted(&b, r.Body)
	}
	for _, c := range fb.Comments {
		if c.Path != "" {
			fmt.Fprintf(&b, "\nComment by %s on %s:%d\n", c.Author, c.Path, c.Line)
		} else {
			fmt.Fprintf(&b, "\nComment by %s\n", c.Author)
		}
		writeQuoted(&b, c.Body)
	}
	return strings.TrimRight(b.String(), "\n")
}

// writeQuoted indents a body two spaces, line by line.
func writeQuoted(b *strings.Builder, body string) {
	for _, line := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
		b.WriteString("  " + line + "\n")
	}
}

// renderAbandon says what is gone, and what refused to go.
func renderAbandon(res workdir.AbandonResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Abandoned %s: the branch %s and its edits are gone.", res.Slug, res.Branch)
	if res.PRNumber > 0 {
		fmt.Fprintf(&b, " Pull request #%d is closed.", res.PRNumber)
	}
	if res.Note != "" {
		b.WriteString(" Left to a maintainer: " + res.Note + ".")
	}
	return b.String()
}

// maxTranslationRows bounds each list translation_status prints. The counts
// above the lists carry the size of the problem; the rows carry where to
// start.
const maxTranslationRows = 30

// renderTranslationStatus is the fi/en report: what has no pair, what
// drifted, what cannot pair at all.
func renderTranslationStatus(s workdir.TranslationStatus, filter string) string {
	scope := "the content tree"
	if filter != "" {
		scope = filter
	}
	if s.Total == 0 {
		return "No pages under " + scope + "."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d pages under %s: %d missing their other language, %d pairs drifted apart, %d with no translationKey.\n",
		s.Total, scope, len(s.Missing), len(s.Stale), len(s.Unkeyed))

	if len(s.Missing) > 0 {
		b.WriteString("\nOnly one language:\n")
		for i, p := range s.Missing {
			if i == maxTranslationRows {
				fmt.Fprintf(&b, "  ... and %d more\n", len(s.Missing)-i)
				break
			}
			fmt.Fprintf(&b, "  %s (%s only)\n", p.Path, p.Lang)
		}
	}
	if len(s.Stale) > 0 {
		b.WriteString("\nDrifted apart, widest gap first:\n")
		for i, pair := range s.Stale {
			if i == maxTranslationRows {
				fmt.Fprintf(&b, "  ... and %d more\n", len(s.Stale)-i)
				break
			}
			fmt.Fprintf(&b, "  %s: %s (%s) was edited %s; %s (%s) not since %s\n",
				pair.Key,
				pair.Newer.Path, pair.Newer.Lang, pair.Newer.At.Format("2006-01-02"),
				pair.Older.Path, pair.Older.Lang, pair.Older.At.Format("2006-01-02"))
		}
	}
	if len(s.Unkeyed) > 0 {
		b.WriteString("\nNo translationKey, so no pairing:\n")
		for i, p := range s.Unkeyed {
			if i == maxTranslationRows {
				fmt.Fprintf(&b, "  ... and %d more\n", len(s.Unkeyed)-i)
				break
			}
			b.WriteString("  " + p + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
