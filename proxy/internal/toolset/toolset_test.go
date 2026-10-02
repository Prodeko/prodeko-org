package toolset

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prodeko/prodeko-org/proxy/internal/fence"
	"github.com/prodeko/prodeko-org/proxy/internal/lint"
	"github.com/prodeko/prodeko-org/proxy/internal/mcpserver"
	"github.com/prodeko/prodeko-org/proxy/internal/workdir"
)

func testToolset(t *testing.T) *Toolset {
	t.Helper()
	mgr, err := workdir.New(workdir.Config{
		RepoPath:  t.TempDir(),
		StateDir:  t.TempDir(),
		Committer: workdir.Author{Name: "Prodeko media bot", Email: "media-bot@prodeko.org"},
	})
	if err != nil {
		t.Fatalf("workdir.New: %v", err)
	}
	ts, err := New(Config{Workdir: mgr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return ts
}

func TestNewRequiresAWorkdir(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("New accepted a configuration with no workdir")
	}
}

// Sixteen tools, named exactly as the design names them. A rename breaks every
// saved connector, so the names are asserted rather than assumed.
func TestTheSixteenTools(t *testing.T) {
	want := []string{
		ToolGetConventions, ToolListFiles, ToolReadFile, ToolSearch,
		ToolWriteFile, ToolEditFile, ToolBuild, ToolRender, ToolScreenshot,
		ToolSubmit, ToolListMyChanges, ToolResumeChange, ToolBeginImageUpload,
		ToolGetFeedback, ToolTranslationStatus, ToolAbandonChange,
	}
	got := testToolset(t).Tools()
	if len(got) != len(want) {
		t.Fatalf("Tools() has %d tools, want %d", len(got), len(want))
	}
	for i, tool := range got {
		if tool.Name != want[i] {
			t.Errorf("tool %d is %q, want %q", i, tool.Name, want[i])
		}
		if tool.Description == "" {
			t.Errorf("tool %q has no description; it is what the model reads", tool.Name)
		}
		if tool.Call == nil {
			t.Errorf("tool %q has no Call", tool.Name)
		}
	}
}

// The schemas are the argument contract. A client validates against them, so
// every one has to be a closed object schema.
func TestSchemasAreClosedObjectSchemas(t *testing.T) {
	required := map[string][]string{
		ToolGetConventions:    nil,
		ToolListFiles:         nil,
		ToolReadFile:          {"path"},
		ToolSearch:            {"pattern"},
		ToolWriteFile:         {"path", "content"},
		ToolEditFile:          {"path", "old", "new"},
		ToolBuild:             nil,
		ToolRender:            {"path"},
		ToolScreenshot:        {"path"},
		ToolSubmit:            {"title"},
		ToolListMyChanges:     nil,
		ToolResumeChange:      nil,
		ToolBeginImageUpload:  nil,
		ToolGetFeedback:       nil,
		ToolTranslationStatus: nil,
		ToolAbandonChange:     {"slug"},
	}

	for _, tool := range testToolset(t).Tools() {
		var schema struct {
			Schema               string                     `json:"$schema"`
			Type                 string                     `json:"type"`
			Properties           map[string]json.RawMessage `json:"properties"`
			Required             []string                   `json:"required"`
			AdditionalProperties *bool                      `json:"additionalProperties"`
		}
		if err := json.Unmarshal(tool.Schema, &schema); err != nil {
			t.Errorf("%s: schema is not valid JSON: %v", tool.Name, err)
			continue
		}
		if schema.Type != "object" {
			t.Errorf("%s: schema type = %q, want object", tool.Name, schema.Type)
		}
		if !strings.Contains(schema.Schema, "json-schema.org") {
			t.Errorf("%s: schema names no $schema dialect", tool.Name)
		}
		if schema.AdditionalProperties == nil || *schema.AdditionalProperties {
			t.Errorf("%s: schema is open; a misspelled argument would be dropped silently", tool.Name)
		}
		if strings.Join(schema.Required, ",") != strings.Join(required[tool.Name], ",") {
			t.Errorf("%s: required = %v, want %v", tool.Name, schema.Required, required[tool.Name])
		}
		for _, name := range schema.Required {
			if _, ok := schema.Properties[name]; !ok {
				t.Errorf("%s: %q is required but not a property", tool.Name, name)
			}
		}
		for name, raw := range schema.Properties {
			var prop struct {
				Type        string `json:"type"`
				Description string `json:"description"`
			}
			if err := json.Unmarshal(raw, &prop); err != nil {
				t.Errorf("%s.%s: property is not valid JSON: %v", tool.Name, name, err)
				continue
			}
			if prop.Type == "" {
				t.Errorf("%s.%s: property has no type", tool.Name, name)
			}
			if prop.Description == "" {
				t.Errorf("%s.%s: property has no description", tool.Name, name)
			}
		}
	}
}

// The argument structs and the schemas are one contract; a property the Go
// side cannot receive is a silent dropped argument.
func TestArgumentStructsMatchTheSchemas(t *testing.T) {
	cases := []struct {
		name string
		args string
		into any
		want any
	}{
		{ToolListFiles, `{"glob":"site/content/fi/**"}`, &listFilesArgs{}, &listFilesArgs{Glob: "site/content/fi/**"}},
		{ToolReadFile, `{"path":"a.md","start":3,"end":9}`, &readFileArgs{}, &readFileArgs{Path: "a.md", Start: 3, End: 9}},
		{ToolSearch, `{"pattern":"x","glob":"*.css","max_results":5}`, &searchArgs{}, &searchArgs{Pattern: "x", Glob: "*.css", MaxResults: 5}},
		{ToolWriteFile, `{"path":"a.md","content":"hi"}`, &writeFileArgs{}, &writeFileArgs{Path: "a.md", Content: "hi"}},
		{ToolEditFile, `{"path":"a.css","old":"red","new":"var(--text-heading)"}`, &editFileArgs{}, &editFileArgs{Path: "a.css", Old: ptr("red"), New: ptr("var(--text-heading)")}},
		{ToolRender, `{"path":"/fi/tapahtumat/","selector":".site-header"}`, &renderArgs{}, &renderArgs{Path: "/fi/tapahtumat/", Selector: ".site-header"}},
		{ToolScreenshot, `{"path":"site/content/fi/tapahtumat.md","width":390}`, &screenshotArgs{}, &screenshotArgs{Path: "site/content/fi/tapahtumat.md", Width: 390}},
		{ToolSubmit, `{"title":"Sininen otsikko","description":"miksi"}`, &submitArgs{}, &submitArgs{Title: "Sininen otsikko", Description: "miksi"}},
		{ToolResumeChange, `{"pr":26}`, &resumeChangeArgs{}, &resumeChangeArgs{PR: 26}},
	}
	for _, tc := range cases {
		if err := json.Unmarshal([]byte(tc.args), tc.into); err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if got, want := jsonOf(t, tc.into), jsonOf(t, tc.want); got != want {
			t.Errorf("%s: decoded %s, want %s", tc.name, got, want)
		}
	}
}

// Every schema is closed, so a misspelled argument has to come back as one. A
// model that sent old_text must not be told its old text was empty.
func TestDecodeRefusesAnUndeclaredProperty(t *testing.T) {
	if _, err := decode[editFileArgs](json.RawMessage(`{"path":"a.css","old_text":"red","new_text":"blue"}`)); err == nil {
		t.Error("decode accepted undeclared properties")
	} else if !strings.Contains(err.Error(), "old_text") {
		t.Errorf("the error does not name the offending property: %v", err)
	}
	if _, err := decode[editFileArgs](json.RawMessage(`{"path":"a.css","old":"red","new":""}`)); err != nil {
		t.Errorf("decode of an empty replacement: %v", err)
	}
}

func ptr(s string) *string { return &s }

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// The tool set has to satisfy the transport's own checks, or the server never
// starts.
func TestToolsAreAcceptedByTheTransport(t *testing.T) {
	ts := testToolset(t)
	_, err := mcpserver.New(mcpserver.Config{
		Name:         "prodeko-content-editor",
		Version:      "test",
		Instructions: ts.Instructions(),
		Tools:        ts.Tools(),
		Authenticate: mcpserver.StaticBearer("secret", mcpserver.Identity{Username: "dev-editor"}),
	})
	if err != nil {
		t.Fatalf("mcpserver.New: %v", err)
	}
}

// get_conventions is the one tool that needs no worktree, and a client that
// drops the initialize instructions depends on it.
func TestGetConventionsReturnsTheGuide(t *testing.T) {
	ts := testToolset(t)
	res, err := ts.Tools()[0].Call(context.Background(), mcpserver.Identity{Username: "maija"}, nil)
	if err != nil {
		t.Fatalf("get_conventions: %v", err)
	}
	got := res.Text
	for _, want := range []string{
		"site/content-members/", "translationKey", "site/assets/css/tokens", "draft pull request",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("conventions do not mention %q", want)
		}
	}
	if ts.Instructions() != got {
		t.Error("initialize instructions and get_conventions disagree")
	}
}

// get_conventions is the server's only statement of what may be edited, so it
// has to be generated from the fence rather than written beside it: a guide that
// drifts from the allowlist tells the model it may edit what it may not.
func TestGetConventionsStatesTheFence(t *testing.T) {
	got := testToolset(t).Instructions()
	for _, r := range fence.Rules() {
		if !strings.Contains(got, r.Prefix+"**") {
			t.Errorf("the guide does not name the root %q", r.Prefix)
		}
		if !strings.Contains(got, r.What) {
			t.Errorf("the guide does not say what lives in %q", r.Prefix)
		}
	}
	for _, g := range fence.LayoutGroups() {
		if !strings.Contains(got, g.Paths) {
			t.Errorf("the guide does not name the template group %q", g.Paths)
		}
		// The reasons are wrapped into the listing, so the whole line is not
		// there to look for; the first words of it are enough to tell whether it
		// was printed at all.
		if head := firstWords(g.Why, 5); !strings.Contains(got, head) {
			t.Errorf("the guide does not give the reason for %q (looked for %q)", g.Paths, head)
		}
	}

	// The gates are enforcement rather than advice, and this is where the server
	// says what it enforces.
	for _, want := range []string{ToolBuild, ToolScreenshot, "site/layouts/"} {
		if !strings.Contains(got, want) {
			t.Errorf("the guide does not mention %q", want)
		}
	}
}

// Where a conversation's edits land is decided by the server, so the guide is
// the only place the model can learn it. A guide that says nothing about it
// leaves the model to guess that yesterday's change is still open, which is
// exactly what the server refuses to do.
func TestGetConventionsStatesTheChangeLifecycle(t *testing.T) {
	// Folded, because where a sentence happens to begin is the guide's business
	// and not part of what it has to say.
	got := strings.ToLower(testToolset(t).Instructions())
	for _, want := range []string{
		// The two tools that decide what a conversation works on, named so the
		// model reaches for them instead of editing afresh.
		ToolResumeChange, ToolListMyChanges,
		// The lifecycle itself: nothing open until an edit, and finished work
		// gone without asking.
		"first edit", "merged",
		// The cue a person actually gives when they mean the old change.
		"jatka",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the guide does not mention %q", want)
		}
	}
}

func firstWords(text string, n int) string {
	words := strings.Fields(text)
	if len(words) > n {
		words = words[:n]
	}
	return strings.Join(words, " ")
}

// Wrapping is what makes the generated part of the guide read like the written
// part; a word longer than the measure still gets a line of its own rather than
// being cut in half.
func TestWrap(t *testing.T) {
	got := wrap("the shell every page is rendered into, head to scripts", 20)
	want := []string{"the shell every page", "is rendered into,", "head to scripts"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("wrap = %q, want %q", got, want)
	}
	for _, line := range wrap("site/layouts/partials/lang-switch.html is fenced", 10) {
		if line == "" {
			t.Error("wrap produced an empty line")
		}
	}
	if got := wrap("   ", 10); got != nil {
		t.Errorf("wrap of nothing = %q, want nothing", got)
	}
}

// call runs one tool and returns the prose it answered with. Every tool but
// screenshot answers in prose alone, so the pictures are asserted where they
// are produced rather than in every caller here.
func call(t *testing.T, ts *Toolset, name string, id mcpserver.Identity, args string) (string, error) {
	t.Helper()
	res, err := callTool(t, ts, name, id, args)
	return res.Text, err
}

func callTool(t *testing.T, ts *Toolset, name string, id mcpserver.Identity, args string) (mcpserver.Result, error) {
	t.Helper()
	for _, tool := range ts.Tools() {
		if tool.Name == name {
			return tool.Call(context.Background(), id, json.RawMessage(args))
		}
	}
	t.Fatalf("no tool named %q", name)
	return mcpserver.Result{}, nil
}

var maija = mcpserver.Identity{Username: "maija", Name: "Maija Meikäläinen", Email: "maija@prodeko.org"}

// The fence is the whole of the authorisation, and its lexical half runs
// before a worktree exists: a path under no rule is refused without a clone,
// with the reason named.
func TestPathsOutsideTheFenceAreRefused(t *testing.T) {
	ts := testToolset(t)
	cases := []struct {
		name string
		tool string
		args string
		want error
	}{
		{"write the build configuration", ToolWriteFile, `{"path":"site/hugo.toml","content":"x"}`, fence.ErrOutside},
		{"write a workflow", ToolWriteFile, `{"path":".github/workflows/preview.yml","content":"x"}`, fence.ErrOutside},
		{"write the editor configuration", ToolWriteFile, `{"path":"site/static/admin/config.yml","content":"x"}`, fence.ErrOutside},
		{"write the tree check", ToolEditFile, `{"path":"site/check-trees.sh","old":"a","new":"b"}`, fence.ErrOutside},
		{"write a template", ToolWriteFile, `{"path":"site/layouts/index.html","content":"x"}`, fence.ErrReadOnly},
		{"edit a template", ToolEditFile, `{"path":"site/layouts/index.html","old":"a","new":"b"}`, fence.ErrReadOnly},
		{"write the page skeleton", ToolWriteFile, `{"path":"site/layouts/baseof.html","content":"x"}`, fence.ErrReadOnly},
		{"write the head", ToolEditFile, `{"path":"site/layouts/partials/head.html","old":"a","new":"b"}`, fence.ErrReadOnly},
		{"write a shortcode", ToolWriteFile, `{"path":"site/layouts/_shortcodes/ilmo.html","content":"x"}`, fence.ErrReadOnly},
		{"write a render hook", ToolWriteFile, `{"path":"site/layouts/_markup/render-image.html","content":"x"}`, fence.ErrReadOnly},
		{"read a workflow", ToolReadFile, `{"path":".github/workflows/preview.yml"}`, fence.ErrOutside},
		{"traverse out", ToolReadFile, `{"path":"site/content/../../etc/passwd"}`, fence.ErrBadPath},
		{"traverse out encoded", ToolReadFile, `{"path":"site/content/%2e%2e/etc/passwd"}`, fence.ErrBadPath},
		{"absolute", ToolWriteFile, `{"path":"/etc/passwd","content":"x"}`, fence.ErrBadPath},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := call(t, ts, tc.tool, maija, tc.args)
			if !errors.Is(err, tc.want) {
				t.Fatalf("%s = %v, want %v", tc.tool, err, tc.want)
			}
		})
	}
}

// Templates are readable, and the read path must not pick up the write rule:
// "the events header" only resolves to a selector by reading the template.
func TestTemplatesAreReadable(t *testing.T) {
	_, err := call(t, testToolset(t), ToolReadFile, maija, `{"path":"site/layouts/index.html"}`)
	if errors.Is(err, fence.ErrOutside) || errors.Is(err, fence.ErrReadOnly) {
		t.Fatalf("read_file of a template was refused by the fence: %v", err)
	}
}

// The partials and the page layouts are the point of this iteration: a write to
// one has to get past the fence and fail on something else entirely, which here
// is the fixture's empty repository.
func TestWritableTemplatesPassTheFence(t *testing.T) {
	ts := testToolset(t)
	for _, args := range []string{
		`{"path":"site/layouts/partials/header.html","content":"<header></header>"}`,
		`{"path":"site/layouts/partials/uusi-nosto.html","content":"<div></div>"}`,
		`{"path":"site/layouts/home.html","content":"{{ define \"main\" }}{{ end }}"}`,
		`{"path":"site/layouts/section.html","content":"{{ define \"main\" }}{{ end }}"}`,
		`{"path":"site/layouts/page.html","content":"{{ define \"main\" }}{{ end }}"}`,
	} {
		_, err := call(t, ts, ToolWriteFile, maija, args)
		if errors.Is(err, fence.ErrOutside) || errors.Is(err, fence.ErrReadOnly) {
			t.Errorf("the fence refused %s: %v", args, err)
		}
	}
}

func TestArgumentsAreRefusedBeforeAWorktreeIsOpened(t *testing.T) {
	ts := testToolset(t)
	big := strings.Repeat("a", fence.MaxTextBytes+1)
	cases := []struct {
		name string
		tool string
		args string
		want string
	}{
		{"end before start", ToolReadFile, `{"path":"site/assets/css/main.css","start":40,"end":9}`, "before start"},
		{"negative line", ToolReadFile, `{"path":"site/assets/css/main.css","start":-1}`, "numbered from 1"},
		{"empty pattern", ToolSearch, `{"pattern":"  "}`, "pattern is empty"},
		{"unusable pattern", ToolSearch, `{"pattern":"(unclosed"}`, "regular expression"},
		{"empty title", ToolSubmit, `{"title":"   "}`, "title is empty"},
		{"long title", ToolSubmit, `{"title":"` + strings.Repeat("o", MaxTitleLen+1) + `"}`, "at most"},
		{"long description", ToolSubmit, `{"title":"Otsikko","description":"` + strings.Repeat("o", MaxDescriptionLen+1) + `"}`, "at most"},
		{"empty old text", ToolEditFile, `{"path":"site/assets/css/main.css","old":"","new":"x"}`, "empty"},
		{"a replacement that replaces nothing", ToolEditFile, `{"path":"site/assets/css/main.css","old":"x","new":"x"}`, "the text it replaces"},
		{"oversized write", ToolWriteFile, `{"path":"site/content/fi/x.md","content":"` + big + `"}`, "at most"},
		{"unparsable arguments", ToolReadFile, `{"path":3}`, "read_file"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := call(t, ts, tc.tool, maija, tc.args)
			if err == nil {
				t.Fatalf("%s accepted %s", tc.tool, tc.args)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("%s error %q does not mention %q", tc.tool, err, tc.want)
			}
		})
	}
}

func TestOversizedWriteNamesTheSizeLimit(t *testing.T) {
	_, err := call(t, testToolset(t), ToolWriteFile, maija,
		`{"path":"site/content/fi/x.md","content":"`+strings.Repeat("a", fence.MaxTextBytes+1)+`"}`)
	if !errors.Is(err, fence.ErrTooLarge) {
		t.Fatalf("write_file over the limit = %v, want ErrTooLarge", err)
	}
}

// The branch namespace and the commit author both come from the verified
// identity. Without one there is no change to open and no commit to author.
func TestEveryToolNeedsAnIdentity(t *testing.T) {
	ts := testToolset(t)
	cases := map[string]string{
		ToolListFiles:     `{}`,
		ToolReadFile:      `{"path":"site/content/fi/index.md"}`,
		ToolSearch:        `{"pattern":"blue"}`,
		ToolWriteFile:     `{"path":"site/content/fi/index.md","content":"x"}`,
		ToolEditFile:      `{"path":"site/content/fi/index.md","old":"a","new":"b"}`,
		ToolBuild:         `{}`,
		ToolRender:        `{"path":"/fi/"}`,
		ToolScreenshot:    `{"path":"/fi/"}`,
		ToolSubmit:        `{"title":"Otsikko"}`,
		ToolListMyChanges: `{}`,
	}
	for name, args := range cases {
		_, err := call(t, ts, name, mcpserver.Identity{}, args)
		if err == nil || !strings.Contains(err.Error(), "username") {
			t.Errorf("%s with no identity = %v, want a refusal naming the username", name, err)
		}
	}

	// get_conventions is the one tool that needs neither identity nor tree.
	if _, err := call(t, ts, ToolGetConventions, mcpserver.Identity{}, `{}`); err != nil {
		t.Errorf("get_conventions with no identity: %v", err)
	}
}

func TestAuthorComesFromTheIdentity(t *testing.T) {
	got, err := authorOf(maija)
	if err != nil {
		t.Fatalf("authorOf: %v", err)
	}
	if want := "Maija Meikäläinen <maija@prodeko.org>"; got.String() != want {
		t.Errorf("author = %q, want %q", got, want)
	}

	// A name is optional; an address is not, because the alternative is
	// committing a media person's change in the bot's name.
	got, err = authorOf(mcpserver.Identity{Username: "maija", Email: "maija@prodeko.org"})
	if err != nil {
		t.Fatalf("authorOf without a name: %v", err)
	}
	if got.Name != "maija" {
		t.Errorf("author name = %q, want the username", got.Name)
	}
	if _, err := authorOf(mcpserver.Identity{Username: "maija", Name: "Maija"}); err == nil {
		t.Error("authorOf accepted an identity with no email address")
	}
}

func TestHintFor(t *testing.T) {
	cases := map[string]string{
		"site/content/fi/tapahtumat.md":        "tapahtumat",
		"site/content/fi/tapahtumat/_index.md": "tapahtumat",
		"site/content/fi/tapahtumat/index.md":  "tapahtumat",
		"site/assets/css/main.css":             "main",
		"site/data/hallitus-2026.yaml":         "hallitus-2026",
	}
	for rel, want := range cases {
		if got := hintFor(rel); got != want {
			t.Errorf("hintFor(%q) = %q, want %q", rel, got, want)
		}
	}
}

// Clients differ on whether a tool that takes no arguments is called with an
// empty object, a null or nothing at all.
func TestDecodeAcceptsAbsentArguments(t *testing.T) {
	for _, args := range []string{"", "{}", "null", "  "} {
		got, err := decode[listFilesArgs](json.RawMessage(args))
		if err != nil {
			t.Errorf("decode(%q): %v", args, err)
			continue
		}
		if got.Glob != "" {
			t.Errorf("decode(%q) invented a glob %q", args, got.Glob)
		}
	}
}

func TestRenderFiles(t *testing.T) {
	got := renderFiles([]string{"site/content/fi/index.md", "site/assets/css/main.css"}, "")
	for _, want := range []string{"site/content/fi/index.md", "site/assets/css/main.css", "2 files"} {
		if !strings.Contains(got, want) {
			t.Errorf("renderFiles omits %q:\n%s", want, got)
		}
	}
	if got := renderFiles(nil, "site/data/*.yaml"); !strings.Contains(got, "site/data/*.yaml") {
		t.Errorf("an empty result does not name the glob: %s", got)
	}
}

func TestRenderMatches(t *testing.T) {
	matches := []workdir.Match{
		{Path: "site/assets/css/main.css", Line: 412, Text: ".events-header { color: var(--text-heading) }"},
	}
	got := renderMatches(matches, "events-header", 100)
	if want := "site/assets/css/main.css:412: .events-header"; !strings.Contains(got, want) {
		t.Errorf("renderMatches = %q, want it to contain %q", got, want)
	}
	if strings.Contains(got, "Stopped at") {
		t.Error("renderMatches claims a cap it did not reach")
	}

	// At the cap the model is told to narrow the search rather than left to
	// believe it has seen everything.
	capped := make([]workdir.Match, 3)
	for i := range capped {
		capped[i] = workdir.Match{Path: "a.css", Line: i + 1, Text: "x"}
	}
	if got := renderMatches(capped, "x", 3); !strings.Contains(got, "Stopped at 3 matches") {
		t.Errorf("a capped result does not say so:\n%s", got)
	}

	// One very long line cannot fill the context on its own.
	long := []workdir.Match{{Path: "site/data/x.yaml", Line: 1, Text: strings.Repeat("y", maxMatchLine*2)}}
	if got := renderMatches(long, "y", 100); len(got) > maxMatchLine+100 {
		t.Errorf("a long line was not shortened: %d bytes", len(got))
	}

	if got := renderMatches(nil, "sininen", 100); !strings.Contains(got, "No line matches") {
		t.Errorf("an empty result reads as %q", got)
	}
}

// A failing build is handed back verbatim: a Hugo template error is exactly
// what the model has to read.
func TestRenderBuild(t *testing.T) {
	const hugoErr = `ERROR render of "/fi/tapahtumat/" failed: unclosed action`
	got := renderBuild(workdir.Result{OK: false, Output: hugoErr, Duration: 312 * time.Millisecond})
	if !strings.Contains(got, hugoErr) {
		t.Errorf("renderBuild paraphrased the failure:\n%s", got)
	}
	if !strings.Contains(got, "failed") {
		t.Errorf("renderBuild does not say the build failed:\n%s", got)
	}

	ok := renderBuild(workdir.Result{OK: true, Duration: 220 * time.Millisecond})
	if !strings.Contains(ok, "OK") || !strings.Contains(ok, "220ms") {
		t.Errorf("renderBuild of a clean build = %q", ok)
	}
	if strings.Contains(ok, "stylesheet") || strings.Contains(ok, "pages") {
		t.Errorf("a clean build reports checks that found nothing:\n%s", ok)
	}
}

// The checks' findings are quoted the way they were found, after hugo's words
// and never instead of them. They say nothing about whether the site built: that
// is hugo's answer, and it stands.
func TestRenderBuildQuotesTheFindings(t *testing.T) {
	got := renderBuild(workdir.Result{
		OK:       true,
		Duration: 310 * time.Millisecond,
		CSS: []lint.Finding{{
			Where: "site/assets/css/main.css", Line: 412,
			Text: "a } here closes nothing; everything after it is read as a selector",
		}},
		HTML: []lint.Finding{{
			Where: "fi/tapahtumat/index.html", Line: 88,
			Text: "<div> is never closed, so the browser guesses where it ends",
		}},
	})
	for _, want := range []string{
		"Build OK",
		"site/assets/css/main.css:412: a } here closes nothing",
		"fi/tapahtumat/index.html:88: <div> is never closed",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("renderBuild omits %q:\n%s", want, got)
		}
	}
	// One finding is one thing, not "1 things".
	if strings.Contains(got, "1 things") {
		t.Errorf("renderBuild counts one finding as several:\n%s", got)
	}

	// A failing build says so first; the findings are still worth reading.
	failed := renderBuild(workdir.Result{
		Output: `ERROR render of "/fi/" failed: unclosed action`,
		CSS:    []lint.Finding{{Where: "site/assets/css/main.css", Line: 9, Text: "a { opens here and is never closed"}},
	})
	if !strings.Contains(failed, "Build failed") || !strings.Contains(failed, "unclosed action") {
		t.Errorf("a failed build does not lead with hugo's words:\n%s", failed)
	}
	if !strings.Contains(failed, "main.css:9") {
		t.Errorf("a failed build drops the stylesheet findings:\n%s", failed)
	}
}

func TestRenderSubmit(t *testing.T) {
	got := renderSubmit(workdir.SubmitResult{
		Branch:     "media/maija/tapahtumat",
		Commit:     "0f1c2d3",
		Files:      []string{"site/content/fi/tapahtumat.md"},
		PRNumber:   47,
		PRURL:      "https://github.com/prodeko/prodeko-org/pull/47",
		PreviewURL: workdir.PreviewURL(47),
	})
	for _, want := range []string{"#47", "https://pr-47.preview.prodeko.org/", "media/maija/tapahtumat", "site/content/fi/tapahtumat.md", "about a minute"} {
		if !strings.Contains(got, want) {
			t.Errorf("renderSubmit omits %q:\n%s", want, got)
		}
	}

	// A dry run must not read as a published pull request.
	dry := renderSubmit(workdir.SubmitResult{
		Branch:   "media/maija/tapahtumat",
		DryRun:   true,
		Diffstat: " 1 file changed, 2 insertions(+)",
		Note:     "Dry run: with no GITHUB_TOKEN the branch was pushed to this server's origin only, and no pull request was opened.",
	})
	if strings.Contains(dry, "pull request #") {
		t.Errorf("a dry run reads as a pull request:\n%s", dry)
	}
	for _, want := range []string{"Dry run: with no GITHUB_TOKEN", "1 file changed"} {
		if !strings.Contains(dry, want) {
			t.Errorf("renderSubmit omits %q:\n%s", want, dry)
		}
	}
	// One statement about what the push did, not two differently worded ones.
	if n := strings.Count(strings.ToLower(dry), "dry run"); n != 1 {
		t.Errorf("a dry run says so %d times, want once:\n%s", n, dry)
	}
}

func TestRenderChanges(t *testing.T) {
	if got := renderChanges(nil); !strings.Contains(got, "no open changes") {
		t.Errorf("no changes reads as %q", got)
	}
	got := renderChanges([]workdir.Info{{
		Slug:       "tapahtumat",
		Branch:     "media/maija/tapahtumat",
		UpdatedAt:  time.Date(2026, 9, 19, 14, 2, 0, 0, time.UTC),
		Files:      []string{"site/content/fi/tapahtumat.md"},
		Dirty:      true,
		PRNumber:   47,
		PRURL:      "https://github.com/prodeko/prodeko-org/pull/47",
		PreviewURL: workdir.PreviewURL(47),
		CIState:    "success",
	}})
	for _, want := range []string{"tapahtumat", "media/maija/tapahtumat", "#47", "success", "pr-47.preview.prodeko.org", "2026-09-19", "never submitted"} {
		if !strings.Contains(got, want) {
			t.Errorf("renderChanges omits %q:\n%s", want, got)
		}
	}
}

// Without a GitHub token there is never a pull request to report, so the
// absence of one cannot be what "not submitted yet" is read from: a committed
// dry-run change would tell the editor to submit again.
func TestRenderChangesTellsCommittedFromUnsubmitted(t *testing.T) {
	base := workdir.Info{Slug: "sininen", Branch: "media/maija/sininen"}

	committed := base
	committed.Files = []string{"site/assets/css/tokens/colors.css"}
	if got := renderChanges([]workdir.Info{committed}); !strings.Contains(got, "committed to the branch") || strings.Contains(got, "not submitted yet") {
		t.Errorf("a committed change with no pull request reads as:\n%s", got)
	}

	uncommitted := committed
	uncommitted.Dirty = true
	if got := renderChanges([]workdir.Info{uncommitted}); !strings.Contains(got, "not submitted yet") {
		t.Errorf("a dirty change reads as:\n%s", got)
	}

	if got := renderChanges([]workdir.Info{base}); !strings.Contains(got, "not submitted yet") {
		t.Errorf("an empty change reads as:\n%s", got)
	}
}

// The realm hands out email addresses as usernames; the namespace must come
// out branch-safe and deterministic.
func TestUserOfDerivesABranchSafeName(t *testing.T) {
	for raw, want := range map[string]string{
		"rvirtaha@hotmail.com":          "rvirtaha-hotmail.com",
		"Maija.Meikäläinen@prodeko.org": "maija.meik-l-inen-prodeko.org",
		"dev-editor":                    "dev-editor",
		"..@..":                         "",
	} {
		got, err := userOf(mcpserver.Identity{Username: raw})
		if want == "" {
			if err == nil {
				t.Errorf("userOf(%q) accepted, want refusal", raw)
			}
			continue
		}
		if err != nil || got != want {
			t.Errorf("userOf(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
}
