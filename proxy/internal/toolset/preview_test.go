package toolset

import (
	"bytes"
	"errors"
	"image/png"
	"os/exec"
	"strings"
	"testing"

	"github.com/prodeko/prodeko-org/proxy/internal/preview"
)

func TestRenderPageQuotesTheBuiltHTML(t *testing.T) {
	page := preview.Page{Lang: "fi", URL: "/fi/tapahtumat/"}
	const built = "<h1 class=\"events-header\">Tapahtumat</h1>\n"

	got := renderPage(page, "", built)
	if !strings.HasSuffix(got, built) {
		t.Errorf("renderPage does not end in the page itself:\n%s", got)
	}
	if !strings.Contains(got, "/fi/tapahtumat/") {
		t.Errorf("renderPage does not name the page:\n%s", got)
	}

	// A selector is quoted back, because "nothing matched" and "this matched"
	// are answers to a question the model has to recognise as its own.
	if got := renderPage(page, " .events-header ", built); !strings.Contains(got, `".events-header"`) {
		t.Errorf("renderPage does not name the selector:\n%s", got)
	}
}

// A page that grew past the cap is cut, and the cut is named along with the way
// out of it.
func TestRenderPageStopsAtTheCap(t *testing.T) {
	page := preview.Page{URL: "/fi/"}
	// Multi-byte on purpose: a cut in the middle of a character would be a page
	// the model cannot read back.
	big := strings.Repeat("ä", MaxHTMLBytes)

	got := renderPage(page, "", big)
	if len(got) > MaxHTMLBytes+400 {
		t.Errorf("renderPage returned %d bytes, want about the cap", len(got))
	}
	if !strings.Contains(got, "stopped at") || !strings.Contains(got, "selector") {
		t.Errorf("a truncated page does not say so or say what to do:\n%s", got[len(got)-200:])
	}
	if !strings.Contains(got, "ä") || strings.Contains(got, "\uFFFD") {
		t.Error("the cut landed in the middle of a character")
	}
}

func TestRenderShots(t *testing.T) {
	fi := shot{
		page:  preview.Page{Lang: "fi", URL: "/fi/tapahtumat/", TranslationKey: "events"},
		taken: preview.Shot{Width: 1280, Height: 2418},
	}
	en := shot{
		page:  preview.Page{Lang: "en", URL: "/en/events/", TranslationKey: "events"},
		taken: preview.Shot{Width: 1280, Height: 2402},
	}

	// The model sees pictures and no filenames, so the prose has to name them in
	// the order they arrive.
	got := renderShots([]shot{fi, en}, 1280, "")
	for _, want := range []string{"/fi/tapahtumat/", "/en/events/", "1280 by 2418", "events", "order above"} {
		if !strings.Contains(got, want) {
			t.Errorf("renderShots omits %q:\n%s", want, got)
		}
	}
	if strings.Index(got, "/fi/tapahtumat/") > strings.Index(got, "/en/events/") {
		t.Errorf("the pages are not named in the order the pictures follow:\n%s", got)
	}

	// One picture of a page that pairs with another language is worth saying
	// out loud: the second language was not captured, and the model is the one
	// who has to decide whether that matters.
	alone := renderShots([]shot{fi}, 1280, "")
	if !strings.Contains(alone, "events") || !strings.Contains(alone, "alone") {
		t.Errorf("a lone capture of a paired page does not say so:\n%s", alone)
	}

	// A page with no pair says nothing about pairing.
	plain := shot{page: preview.Page{Lang: "fi", URL: "/fi/tietosuoja/"}, taken: preview.Shot{Width: 390, Height: 900}}
	if got := renderShots([]shot{plain}, 390, ""); strings.Contains(got, "translationKey") {
		t.Errorf("a page with no pair is described as one:\n%s", got)
	}

	// A page taller than any capture is not silently cropped.
	tall := shot{page: preview.Page{Lang: "fi", URL: "/fi/"}, taken: preview.Shot{Width: 390, Height: 8000, Cut: true}}
	if got := renderShots([]shot{tall}, 390, ""); !strings.Contains(got, "taller than the capture") {
		t.Errorf("a cut-off capture does not say so:\n%s", got)
	}

	// Bookkeeping that failed travels with the picture rather than replacing it.
	if got := renderShots([]shot{fi}, 1280, "This screenshot could not be recorded."); !strings.Contains(got, "could not be recorded") {
		t.Errorf("the note was dropped:\n%s", got)
	}
}

func TestCaptureWidth(t *testing.T) {
	for width, want := range map[int]int{
		0:                    preview.DesktopWidth,
		preview.DesktopWidth: preview.DesktopWidth,
		preview.MobileWidth:  preview.MobileWidth,
	} {
		got, err := captureWidth(width)
		if err != nil || got != want {
			t.Errorf("captureWidth(%d) = %d, %v; want %d", width, got, err, want)
		}
	}
	// A client that did not validate against the schema is told the two widths
	// rather than given a picture of a third.
	_, err := captureWidth(1440)
	if err == nil || !strings.Contains(err.Error(), "1280") {
		t.Fatalf("captureWidth(1440) = %v, want a refusal naming the widths", err)
	}
}

// The whole path, through the tools a model actually calls: a change is opened,
// built, read back and photographed. Everything below this is unit-tested in
// pieces; this is the one test that the pieces are wired to each other.
func TestRenderAndScreenshotAgainstARealBuild(t *testing.T) {
	ts := toolsetOverARealClone(t)

	// Before a build there is nothing to look at, and the refusal is the one
	// instruction that gets the model out of it.
	if _, err := call(t, ts, ToolRender, maija, `{"path":"/fi/tapahtumat/"}`); err == nil {
		t.Fatal("render answered a change that was never built")
	} else if !strings.Contains(err.Error(), ToolBuild) {
		t.Fatalf("render before a build = %v, want it to name %s", err, ToolBuild)
	}

	if out, err := call(t, ts, ToolBuild, maija, `{}`); err != nil || !strings.Contains(out, "OK") {
		t.Fatalf("build = %q, %v", out, err)
	}

	// The page named by the file the editor would have edited, and the part of
	// it a selector matches.
	out, err := call(t, ts, ToolRender, maija, `{"path":"site/content/fi/tapahtumat.md","selector":"h1.otsikko"}`)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(out, `<h1 class="otsikko">Tapahtumat</h1>`) {
		t.Fatalf("render did not return the heading:\n%s", out)
	}
	if !strings.Contains(out, "/fi/tapahtumat/") {
		t.Fatalf("render does not name the page it read:\n%s", out)
	}

	// The picture needs a browser, which a developer's machine is allowed not to
	// have. Everything above this point has already been asserted.
	if _, err := (preview.Shooter{}).Find(); err != nil {
		t.Skipf("%v: the rest of this test needs a browser", err)
	}

	res, err := callTool(t, ts, ToolScreenshot, maija, `{"path":"site/content/fi/tapahtumat.md"}`)
	if err != nil {
		t.Fatalf("screenshot: %v", err)
	}
	// Both languages in one call, because a layout change is a change to both
	// whether or not anybody remembered to look at the second.
	if len(res.Images) != 2 {
		t.Fatalf("pictures = %d, want the Finnish page and its English pair", len(res.Images))
	}
	for _, want := range []string{"/fi/tapahtumat/", "/en/events/", "1280 px"} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("the prose does not name %q:\n%s", want, res.Text)
		}
	}
	for i, img := range res.Images {
		if _, err := png.Decode(bytes.NewReader(img.PNG)); err != nil {
			t.Errorf("picture %d is not a readable PNG: %v", i, err)
		}
	}
}

// toolsetOverARealClone is the session fixture's tool set, for the tests that
// also need hugo. The behaviour under test is what hugo and git do, so neither
// is mocked; a machine without hugo skips instead.
func toolsetOverARealClone(t *testing.T) *Toolset {
	t.Helper()
	if _, err := exec.LookPath("hugo"); err != nil {
		t.Skip("hugo is not on PATH")
	}
	return newFixture(t).ts
}

// There is one thing to do about a change nobody has built, and the refusal
// names it.
func TestBuildFirstNamesTheBuildTool(t *testing.T) {
	err := buildFirst(ToolScreenshot, preview.ErrNotBuilt)
	if !strings.Contains(err.Error(), ToolBuild) {
		t.Errorf("the refusal does not name %s: %v", ToolBuild, err)
	}

	// Every other refusal already says what to do differently and is passed on
	// as it is.
	other := errors.New("preview: nothing at /fi/ei-ole/")
	if got := buildFirst(ToolRender, other); got != other {
		t.Errorf("buildFirst rewrote an unrelated refusal: %v", got)
	}
}
