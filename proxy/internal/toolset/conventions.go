package toolset

import (
	"fmt"
	"strings"

	"github.com/prodeko/prodeko-org/proxy/internal/fence"
)

// conventionsHead is the first half of what get_conventions returns and what
// rides in the MCP initialize instructions. Most of the guide is advice rather
// than enforcement: the enforcement layer is the fence and a maintainer's
// review. It exists because a model that knows the bilingual pairing and the
// token layer writes changes a maintainer merges, and one that does not writes
// changes a maintainer has to explain.
//
// It is prose rather than a schema on purpose. The model reads it once per
// session, and a list of rules with reasons is what it can act on.
//
// The editable tree between the two halves is generated from the fence, because
// the server states that list exactly once and every other document — this
// guide, the skill, the README — reads it from here.
const conventionsHead = `# Editing prodeko.org

The site is Hugo. Pages are Markdown with YAML front matter, lists of people
and links are YAML data files, and the look is plain CSS. A change you make
here becomes a draft pull request; a maintainer reviews and merges it, and the
site deploys. Nothing you do publishes directly.

## The tree you can see

`

// conventionsTail is everything after the generated tree.
const conventionsTail = `
Everything else is invisible: the build configuration, the workflows, the
Decap editor configuration, the proxy, the tools, the docs. Those decide what
the build is allowed to do and who may edit, so they are not editable from
here. Asking for them is not a mistake worth apologising for; just tell the
person what you cannot reach and why.

Read every template freely, the fenced ones included. "The events header" only
becomes a CSS selector by reading the template that renders it, and a change to
a writable partial is often a change to the stylesheet beside it.

A template you may write is still a template every page goes through: a partial
renders in both languages and on pages nobody asked you to touch. Look at what
you changed with ` + "`" + `screenshot` + "`" + ` before you submit it; the server insists on it.

## Two languages, one page

Finnish lives under ` + "`" + `fi/` + "`" + ` and English under ` + "`" + `en/` + "`" + `, in both content roots.
Two files are the same page in two languages when they share a
` + "`" + `translationKey` + "`" + ` in their front matter; the addresses themselves differ, which
is the point of the key.

A change to a Finnish page usually wants its English pair. Offer it. If the
person only wants one side changed, say which file you left alone, so the
maintainer reviewing the pull request is not left guessing.

Prodeko writes Finnish first. Keep the Finnish page's voice; do not translate
it into English prose with Finnish words.

## Member pages are separate on purpose

` + "`" + `site/content-members/` + "`" + ` builds into a tree that is never served publicly.
Member material is meeting minutes and back issues of the guild magazine.
Moving a page between the two roots changes who can read it: do it only when
that is exactly what was asked for, and say so in the pull request title.

## Colour and spacing come from tokens

` + "`" + `site/assets/css/tokens/` + "`" + ` holds the brand: ` + "`" + `colors.css` + "`" + ` (the three official
blues, the overalls rainbow, and semantic aliases like ` + "`" + `--text-heading` + "`" + ` and
` + "`" + `--surface-card` + "`" + `), plus spacing, radius, typography, elevation and motion.

Use an existing token before you write a hex value. If the person asks for
"a lighter blue", look for the token that already means it rather than
inventing ` + "`" + `#3355aa` + "`" + `. A raw hex value in a diff is the thing a reviewer stops
on. When nothing fits, say so and propose the closest token.

` + "`" + `site/assets/css/main.css` + "`" + ` is large. Search it, read the range you need, and
edit with ` + "`" + `edit_file` + "`" + `; do not read it whole every turn.

## The loop

Search, read, edit, then ` + "`" + `build` + "`" + `. The build is a few hundred milliseconds and
it runs the site's own tree check as well, so a broken shortcode or an
unclosed template action comes back in the same turn that made it. It reads
the built HTML and the stylesheets too, and quotes what it noticed.

Then look at it: ` + "`" + `screenshot` + "`" + ` gives you a picture of the built page, both
languages in one call when the page has a pair, and ` + "`" + `render` + "`" + ` gives you the
markup when the markup is the question. Edit again until it is right.

` + "`" + `submit` + "`" + ` commits everything in the change, authored in the signed-in person's
name, pushes the branch and opens a draft pull request with a preview link.
The preview is ready about a minute later. Asking for another tweak afterwards
continues the same change and updates the same pull request.

Three things ` + "`" + `submit` + "`" + ` insists on, and refuses without:

    a build since the last edit, so nothing broken is proposed;
    for a change touching site/layouts/, a screenshot since the last edit,
      because a template also renders on the pages you were not looking at;
    for a change touching site/layouts/, a description of what looks
      different, which is what a maintainer reads before the diff.

## One conversation, one change

A conversation starts on a clean checkout of the published site. Looking costs
nothing: reading, searching and building open no change, and the first edit is
what opens one. Every edit after that lands in the same change, including the
tweak asked for after ` + "`" + `submit` + "`" + `.

Half-finished work from before is never picked up for you, because inheriting
it silently is how an unrelated file ends up in somebody's pull request.
Continuing it is deliberate: ` + "`" + `resume_change` + "`" + ` takes a slug as
` + "`" + `list_my_changes` + "`" + ` names them, or a pull request number, and from then on edits
land on that change's branch and its pull request.

"Jatka", "sama kuin eilen", or a request to fix what review asked for means an
existing change rather than a second one beside it. Call ` + "`" + `list_my_changes` + "`" + ` and
offer what you find instead of editing afresh. The first answer you get in a
conversation names any open changes by itself, so usually you already know.

If nothing is asked for a long while, or the server restarts, the conversation
stops being tied to its change and the next edit opens a new one. The old change
is untouched and ` + "`" + `resume_change` + "`" + ` still reaches it, which is what makes
` + "`" + `list_my_changes` + "`" + ` worth reading before editing again.

Merged and closed changes disappear on their own, so what is listed is what is
still live. Three may be open at once; a refusal to open a fourth means three
are genuinely unfinished, and submitting or abandoning one is the way out.

## What this cannot do

Images: there is no way to hand file bytes to ` + "`" + `write_file` + "`" + `. Point the person at
the ordinary editing screen for uploads; you can then reference the path they
give you.

Publishing: there is no merge tool. A maintainer merges.

Configuration, workflows and the build: outside the fence, deliberately.
`

// Conventions is the site guide the tools hand to the model. It is returned by
// get_conventions and carried in the MCP initialize instructions, because a
// client is free to ignore either one.
func Conventions() string { return conventionsHead + editableTree() + conventionsTail }

// editableTree is the fence, stated. The roots come from [fence.Rules] and the
// template groups from [fence.LayoutGroups], so what this prints is what the
// server will actually permit: a hand-written copy of the list would be one
// release away from telling the model it may edit something it may not.
func editableTree() string {
	var b strings.Builder
	rules := fence.Rules()
	width := 0
	for _, r := range rules {
		if n := len(r.Prefix) + len("**"); n > width {
			width = n
		}
	}
	for _, r := range rules {
		fmt.Fprintf(&b, "    %-*s  %s\n", width, r.Prefix+"**", r.What)
	}

	var writable, fenced []fence.LayoutGroup
	for _, g := range fence.LayoutGroups() {
		if g.Write {
			writable = append(writable, g)
		} else {
			fenced = append(fenced, g)
		}
	}
	b.WriteString("\n### Templates you may write\n\n")
	writeGroups(&b, writable)
	b.WriteString("\n### Templates that stay read only\n\n")
	writeGroups(&b, fenced)
	return b.String()
}

// writeGroups lays out one group per entry: the paths, then the fence's own
// line about them. The reason is printed rather than summarised, because "why
// not" is what tells the model whether to edit something else or to hand the
// request to a developer.
func writeGroups(b *strings.Builder, groups []fence.LayoutGroup) {
	for _, g := range groups {
		fmt.Fprintf(b, "    %s\n", g.Paths)
		for _, line := range wrap(g.Why, 66) {
			fmt.Fprintf(b, "        %s\n", line)
		}
	}
}

// wrap breaks a line of prose at whitespace, so the generated part of the guide
// reads like the written part instead of running off the edge of it.
func wrap(text string, width int) []string {
	var lines []string
	line := ""
	for _, word := range strings.Fields(text) {
		switch {
		case line == "":
			line = word
		case len(line)+1+len(word) <= width:
			line += " " + word
		default:
			lines = append(lines, line)
			line = word
		}
	}
	if line != "" {
		lines = append(lines, line)
	}
	return lines
}
