package proleko

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"
)

var fakeJPEG = []byte("\xFF\xD8\xFF\xE0cover")

func newTestHandler(t *testing.T) (*Handler, string) {
	t.Helper()
	dir := t.TempDir()
	h, err := New(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	h.now = func() time.Time { return time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC) }
	return h, dir
}

type part struct{ name, file, value string }

func post(t *testing.T, h *Handler, parts ...part) (int, map[string]string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, p := range parts {
		if p.file == "" {
			_ = mw.WriteField(p.name, p.value)
			continue
		}
		w, _ := mw.CreateFormFile(p.name, p.file)
		_, _ = w.Write([]byte(p.value))
	}
	mw.Close()
	req := httptest.NewRequest("POST", Path, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

var (
	year  = part{"year", "", "2026"}
	num   = part{"number", "", "4"}
	pdf   = part{"pdf", "lehti.pdf", "%PDF-1.7\nthe whole issue"}
	cover = part{"cover", "kansi.jpg", string(fakeJPEG)}
)

func TestStoresPDFAndCover(t *testing.T) {
	h, dir := newTestHandler(t)
	code, out := post(t, h, year, num, cover, pdf)
	if code != 200 {
		t.Fatalf("status %d: %v", code, out)
	}
	re := regexp.MustCompile(`^2026/proleko-2026-4-[0-9a-f]{8}\.pdf$`)
	if !re.MatchString(out["pdf"]) {
		t.Fatalf("pdf = %q", out["pdf"])
	}
	if out["cover"] != out["pdf"][:len(out["pdf"])-4]+".jpg" {
		t.Errorf("cover = %q", out["cover"])
	}
	got, err := os.ReadFile(filepath.Join(dir, out["pdf"]))
	if err != nil || !bytes.HasPrefix(got, []byte("%PDF-")) {
		t.Fatalf("PDF not stored: %v", err)
	}
	if c, _ := os.ReadFile(filepath.Join(dir, out["cover"])); !bytes.Equal(c, fakeJPEG) {
		t.Error("cover not stored")
	}
	if left, _ := os.ReadDir(filepath.Join(dir, ".incoming")); len(left) != 0 {
		t.Errorf("temporary files left: %v", left)
	}

	// A second upload of the same issue gets a new name: a draft must not
	// overwrite the PDF the published page links to.
	_, again := post(t, h, year, num, pdf)
	if again["pdf"] == out["pdf"] || again["cover"] != "" {
		t.Errorf("second upload = %v", again)
	}
}

func TestRefusals(t *testing.T) {
	cases := map[string][]part{
		"no year":          {num, pdf},
		"year too old":     {{"year", "", "1899"}, num, pdf},
		"year as a path":   {{"year", "", "../../etc"}, num, pdf},
		"number zero":      {year, {"number", "", "0"}, pdf},
		"number too big":   {year, {"number", "", "21"}, pdf},
		"fields after pdf": {pdf, year, num},
		"not a pdf":        {year, num, {"pdf", "x.pdf", "<html>"}},
		"no pdf":           {year, num, cover},
		"cover not jpeg":   {year, num, {"cover", "k.png", "\x89PNG\r\n\x1a\n"}, pdf},
		"unknown field":    {year, num, {"path", "", "/etc/passwd"}, pdf},
		"two pdfs":         {year, num, pdf, pdf},
	}
	for name, parts := range cases {
		t.Run(name, func(t *testing.T) {
			h, dir := newTestHandler(t)
			code, out := post(t, h, parts...)
			if code != http.StatusBadRequest || out["error"] == "" {
				t.Fatalf("status %d: %v", code, out)
			}
			var files []string
			_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
				if err == nil && !d.IsDir() {
					files = append(files, p)
				}
				return nil
			})
			if len(files) != 0 {
				t.Errorf("files left behind: %v", files)
			}
		})
	}
}

func TestOnlyPost(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", Path, nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: %d", rec.Code)
	}
}

func TestNewChecksDirectory(t *testing.T) {
	if _, err := New(filepath.Join(t.TempDir(), "missing"), nil); err == nil {
		t.Error("a missing directory was accepted")
	}
	if _, err := New("relative/dir", nil); err == nil {
		t.Error("a relative directory was accepted")
	}
}
