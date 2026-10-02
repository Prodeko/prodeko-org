package toolset

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prodeko/prodeko-org/proxy/internal/workdir"
)

// The gate, in the states a session actually reaches. The times are stamps on
// disk, so the only thing that matters about them is their order.
func TestGate(t *testing.T) {
	var (
		zero  time.Time
		one   = time.Date(2026, 9, 20, 11, 0, 0, 0, time.UTC)
		two   = one.Add(time.Minute)
		three = two.Add(time.Minute)
	)
	content := []string{"site/content/fi/tapahtumat.md"}
	layout := []string{"site/layouts/partials/header.html"}
	both := []string{"site/assets/css/main.css", "site/layouts/home.html"}

	cases := []struct {
		name        string
		stamps      workdir.Stamps
		files       []string
		description string
		want        []string // substrings the refusal has to carry; none means it passes
	}{
		{
			name:   "a content change that was built",
			stamps: workdir.Stamps{EditedAt: one, BuiltAt: two},
			files:  content,
		},
		{
			name:   "a content change edited after the build",
			stamps: workdir.Stamps{EditedAt: two, BuiltAt: one},
			files:  content,
			want:   []string{ToolBuild, "since the last edit"},
		},
		{
			name:   "a change nothing has ever been done to",
			stamps: workdir.Stamps{EditedAt: one},
			files:  content,
			want:   []string{ToolBuild},
		},
		{
			// Two stamps in the same instant cannot be put in an order, and the
			// safe reading is that the edit came last.
			name:   "a build and an edit in the same instant",
			stamps: workdir.Stamps{EditedAt: one, BuiltAt: one},
			files:  content,
			want:   []string{ToolBuild},
		},
		{
			// A screenshot is only asked for where a template changed. Content
			// shows itself in the diff.
			name:   "a content change nobody looked at",
			stamps: workdir.Stamps{EditedAt: one, BuiltAt: two},
			files:  content,
		},
		{
			name:        "a template change that was built, looked at and described",
			stamps:      workdir.Stamps{EditedAt: one, BuiltAt: two, ShotAt: three},
			files:       layout,
			description: "Otsikko on nyt sininen.",
		},
		{
			name:        "a template change nobody looked at",
			stamps:      workdir.Stamps{EditedAt: one, BuiltAt: two},
			files:       layout,
			description: "Otsikko on nyt sininen.",
			want:        []string{ToolScreenshot, "site/layouts/partials/header.html"},
		},
		{
			name:        "a template change looked at before the last edit",
			stamps:      workdir.Stamps{EditedAt: two, BuiltAt: three, ShotAt: one},
			files:       layout,
			description: "Otsikko on nyt sininen.",
			want:        []string{ToolScreenshot},
		},
		{
			name:        "a template change with nothing said about it",
			stamps:      workdir.Stamps{EditedAt: one, BuiltAt: two, ShotAt: three},
			files:       layout,
			description: "   ",
			want:        []string{"description", "looks different"},
		},
		{
			// Everything missing at once comes back as one refusal: three
			// separate turns to learn three things is three turns wasted.
			name:   "a template change at the start of the loop",
			stamps: workdir.Stamps{EditedAt: three},
			files:  both,
			want:   []string{"3 things first", ToolBuild, ToolScreenshot, "description", "site/layouts/home.html"},
		},
		{
			name:        "a change touching a template among other files",
			stamps:      workdir.Stamps{EditedAt: one, BuiltAt: two, ShotAt: three},
			files:       both,
			description: "Etusivun nostot ovat nyt kahdessa palstassa.",
		},
		{
			// A stamp that was never written reads as never, and never is what
			// the gate refuses on.
			name:   "a change with no stamps at all",
			stamps: workdir.Stamps{EditedAt: zero, BuiltAt: zero},
			files:  content,
			want:   []string{ToolBuild},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := gate(tc.stamps, tc.files, tc.description)
			if len(tc.want) == 0 {
				if err != nil {
					t.Fatalf("gate refused a finished change: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("gate accepted a change the loop had not finished")
			}
			if !errors.Is(err, ErrNotReady) {
				t.Errorf("gate error is not ErrNotReady: %v", err)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal omits %q:\n%v", want, err)
				}
			}
			// Every refusal ends by naming the tool to come back to, or the
			// model has been told what is wrong and not what to do.
			if !strings.Contains(err.Error(), ToolSubmit) {
				t.Errorf("the refusal does not say to submit again:\n%v", err)
			}
		})
	}
}

// One thing missing is one sentence; a list of one is a list nobody needs.
func TestGateReadsAsProseForASingleMiss(t *testing.T) {
	st := workdir.Stamps{EditedAt: time.Unix(200, 0), BuiltAt: time.Unix(100, 0)}
	err := gate(st, []string{"site/content/fi/x.md"}, "")
	if err == nil {
		t.Fatal("gate accepted an unbuilt change")
	}
	if strings.Contains(err.Error(), "\n") {
		t.Errorf("a single miss is rendered as a list:\n%v", err)
	}
}

func TestLayoutFiles(t *testing.T) {
	got := layoutFiles([]string{
		"site/assets/css/main.css",
		"site/layouts/home.html",
		"site/content/fi/index.md",
		"site/layouts/partials/header.html",
	})
	if want := 2; len(got) != want {
		t.Fatalf("layoutFiles found %d templates, want %d: %v", len(got), want, got)
	}
	for _, f := range got {
		if !strings.HasPrefix(f, "site/layouts/") {
			t.Errorf("layoutFiles returned %q, which is not a template", f)
		}
	}
}
