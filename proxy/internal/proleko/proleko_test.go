package proleko

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prodeko/prodeko-org/proxy/internal/ghauth"
	"github.com/prodeko/prodeko-org/proxy/internal/session"
)

// fakeGitHub answers the calls publish and read make, and remembers what was
// written.
type fakeGitHub struct {
	mu        sync.Mutex
	existing  map[string]string // path → content on the branch
	mergeCode int               // 0 means the merge succeeds
	blobs     map[string][]byte
	tree      []map[string]any
	commit    map[string]any
	refs      []string
	merged    bool
	deleted   []string
}

func (f *fakeGitHub) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q", got)
		}
		p := strings.TrimPrefix(r.URL.Path, "/repos/prodeko/prodeko-org")
		var body map[string]any
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}
		reply := func(code int, v any) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(v)
		}
		switch {
		case r.Method == "GET" && strings.HasPrefix(p, "/contents/"):
			if r.URL.Query().Get("ref") != "main" {
				t.Errorf("read from ref %q", r.URL.Query().Get("ref"))
			}
			c, ok := f.existing[strings.TrimPrefix(p, "/contents/")]
			if !ok {
				reply(404, map[string]string{"message": "Not Found"})
				return
			}
			reply(200, map[string]string{"content": base64.StdEncoding.EncodeToString([]byte(c)), "encoding": "base64"})
		case r.Method == "GET" && p == "/git/ref/heads/main":
			reply(200, map[string]any{"object": map[string]string{"sha": "base"}})
		case r.Method == "GET" && p == "/git/commits/base":
			reply(200, map[string]any{"tree": map[string]string{"sha": "basetree"}})
		case r.Method == "POST" && p == "/git/blobs":
			raw, _ := base64.StdEncoding.DecodeString(body["content"].(string))
			sha := "blob" + string(rune('a'+len(f.blobs)))
			if f.blobs == nil {
				f.blobs = map[string][]byte{}
			}
			f.blobs[sha] = raw
			reply(201, map[string]string{"sha": sha})
		case r.Method == "POST" && p == "/git/trees":
			if body["base_tree"] != "basetree" {
				t.Errorf("base_tree = %v", body["base_tree"])
			}
			for _, e := range body["tree"].([]any) {
				f.tree = append(f.tree, e.(map[string]any))
			}
			reply(201, map[string]string{"sha": "newtree"})
		case r.Method == "POST" && p == "/git/commits":
			f.commit = body
			reply(201, map[string]string{"sha": "newcommit"})
		case r.Method == "POST" && p == "/git/refs":
			f.refs = append(f.refs, body["ref"].(string))
			reply(201, map[string]string{})
		case r.Method == "POST" && p == "/pulls":
			if body["base"] != "main" {
				t.Errorf("pull request base = %v", body["base"])
			}
			reply(201, map[string]any{"number": 7, "html_url": "https://github.com/prodeko/prodeko-org/pull/7"})
		case r.Method == "PUT" && p == "/pulls/7/merge":
			if f.mergeCode != 0 {
				reply(f.mergeCode, map[string]string{"message": "Required review missing"})
				return
			}
			f.merged = true
			reply(200, map[string]any{"merged": true})
		case r.Method == "DELETE" && strings.HasPrefix(p, "/git/refs/heads/"):
			f.deleted = append(f.deleted, strings.TrimPrefix(p, "/git/refs/heads/"))
			w.WriteHeader(204)
		default:
			t.Errorf("unexpected GitHub call %s %s", r.Method, r.URL.Path)
			reply(500, map[string]string{"message": "unexpected"})
		}
	})
}

// treePaths returns the tree entries as path → blob content, with nil for a
// deletion.
func (f *fakeGitHub) treePaths() map[string][]byte {
	out := map[string][]byte{}
	for _, e := range f.tree {
		if e["sha"] == nil {
			out[e["path"].(string)] = nil
			continue
		}
		out[e["path"].(string)] = f.blobs[e["sha"].(string)]
	}
	return out
}

var (
	editor = session.Identity{Subject: "s1", Name: "Eeva Editori", Email: "eeva@example.org", Username: "eeva",
		Roles: []string{"membership", "prodeko-org-admin"}}
	chief = session.Identity{Subject: "s2", Name: "Paavo Päätoimittaja", Email: "paavo@example.org", Username: "paavo",
		Roles: []string{"membership", "proleko-editor"}}
	member = session.Identity{Subject: "s3", Name: "Mikko Jäsen", Email: "mikko@example.org", Username: "mikko",
		Roles: []string{"membership"}}
)

var fakeJPEG = []byte("\xFF\xD8\xFF\xE0rendered-cover")

func newTestHandler(t *testing.T, gh *fakeGitHub) (*Handler, string) {
	t.Helper()
	srv := httptest.NewServer(gh.handler(t))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	h, err := New(Config{
		Dir:          dir,
		EditorRoles:  []string{"membership", "prodeko-org-admin"},
		ProlekoRoles: []string{"membership", "proleko-editor"},
		Owner:        "prodeko", Repo: "prodeko-org", Branch: "main", APIRoot: srv.URL,
		Credential: ghauth.Static("test-token"),
		Committer:  Author{Name: "Prodeko CMS", Email: "cms@prodeko.org"},
		Render: func(ctx context.Context, pdfPath string) ([]byte, error) {
			return fakeJPEG, nil
		},
		Now: func() time.Time { return time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return h, dir
}

type form struct {
	fields map[string]string
	pdf    []byte
	cover  []byte
}

func post(t *testing.T, h *Handler, id *session.Identity, f form) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range f.fields {
		_ = mw.WriteField(k, v)
	}
	if f.pdf != nil {
		w, _ := mw.CreateFormFile("pdf", "lehti.pdf")
		_, _ = w.Write(f.pdf)
	}
	if f.cover != nil {
		w, _ := mw.CreateFormFile("cover", "kansi.png")
		_, _ = w.Write(f.cover)
	}
	mw.Close()
	req := httptest.NewRequest("POST", Path, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if id != nil {
		req = req.WithContext(session.NewContext(req.Context(), *id))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func validForm() form {
	return form{
		fields: map[string]string{"year": "2026", "number": "4", "title": `Joulu-Proleko? "26`},
		pdf:    []byte("%PDF-1.7\nthe whole issue"),
	}
}

func TestChiefAddsIssue(t *testing.T) {
	gh := &fakeGitHub{}
	h, dir := newTestHandler(t, gh)

	code, out := post(t, h, &chief, validForm())
	if code != 200 {
		t.Fatalf("status %d: %v", code, out)
	}
	if out["merged"] != true || out["pdf"] != "/media/proleko/2026/proleko-2026-4.pdf" {
		t.Errorf("result = %v", out)
	}

	pdf, err := os.ReadFile(filepath.Join(dir, "2026", "proleko-2026-4.pdf"))
	if err != nil || !bytes.HasPrefix(pdf, []byte("%PDF-")) {
		t.Fatalf("PDF not in place: %v", err)
	}
	if left, _ := os.ReadDir(filepath.Join(dir, ".incoming")); len(left) != 0 {
		t.Errorf("temporary files left behind: %v", left)
	}

	files := gh.treePaths()
	if len(files) != 2 {
		t.Fatalf("tree = %v", files)
	}
	want := "year: 2026\nnumber: 4\ntitle: \"Joulu-Proleko? \\\"26\"\npdf: 2026/proleko-2026-4.pdf\ncover: images/proleko/kannet/2026-4.jpg\n"
	if got := string(files["site/data/proleko/2026-4.yaml"]); got != want {
		t.Errorf("data file =\n%s\nwant\n%s", got, want)
	}
	if !bytes.Equal(files["site/assets/images/proleko/kannet/2026-4.jpg"], fakeJPEG) {
		t.Errorf("cover was not the rendered first page")
	}

	author := gh.commit["author"].(map[string]any)
	if author["name"] != "Paavo Päätoimittaja" || author["email"] != "paavo@example.org" {
		t.Errorf("author = %v", author)
	}
	if committer := gh.commit["committer"].(map[string]any); committer["email"] != "cms@prodeko.org" {
		t.Errorf("committer = %v", committer)
	}
	if !gh.merged || len(gh.deleted) != 1 || !strings.HasPrefix(gh.deleted[0], "proleko/2026-4-") {
		t.Errorf("merged=%v deleted=%v", gh.merged, gh.deleted)
	}
}

func TestRoles(t *testing.T) {
	gh := &fakeGitHub{}
	h, _ := newTestHandler(t, gh)
	if code, _ := post(t, h, &member, validForm()); code != http.StatusForbidden {
		t.Errorf("member without the roles: %d", code)
	}
	if code, _ := post(t, h, nil, validForm()); code != http.StatusUnauthorized {
		t.Errorf("no session: %d", code)
	}
	if code, out := post(t, h, &editor, validForm()); code != 200 {
		t.Errorf("website editor: %d %v", code, out)
	}
}

// With no PROLEKO_ROLES configured the magazine editor is an ordinary member
// and is refused: an empty set must never admit everyone.
func TestNoProlekoRolesAdmitsEditorsOnly(t *testing.T) {
	gh := &fakeGitHub{}
	h, _ := newTestHandler(t, gh)
	h.cfg.ProlekoRoles = nil
	if code, _ := post(t, h, &chief, validForm()); code != http.StatusForbidden {
		t.Errorf("chief with ProlekoRoles unset: %d", code)
	}
	if code, _ := post(t, h, &member, validForm()); code != http.StatusForbidden {
		t.Errorf("member with ProlekoRoles unset: %d", code)
	}
}

func TestValidation(t *testing.T) {
	cases := map[string]func(*form){
		"year too old":    func(f *form) { f.fields["year"] = "1899" },
		"year far ahead":  func(f *form) { f.fields["year"] = "2031" },
		"year not number": func(f *form) { f.fields["year"] = "../../etc" },
		"number zero":     func(f *form) { f.fields["number"] = "0" },
		"number too big":  func(f *form) { f.fields["number"] = "21" },
		"title newline":   func(f *form) { f.fields["title"] = "a\nb: c" },
		"title too long":  func(f *form) { f.fields["title"] = strings.Repeat("x", 121) },
		"not a pdf":       func(f *form) { f.pdf = []byte("<html>hi</html>") },
		"cover not image": func(f *form) { f.cover = []byte("GIF89a....") },
		"unknown field":   func(f *form) { f.fields["path"] = "/etc/passwd" },
		"new without pdf": func(f *form) { f.pdf = nil },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			gh := &fakeGitHub{}
			h, dir := newTestHandler(t, gh)
			f := validForm()
			mutate(&f)
			code, out := post(t, h, &chief, f)
			if code != http.StatusBadRequest {
				t.Fatalf("status %d: %v", code, out)
			}
			if out["error"] == "" {
				t.Errorf("no message for the editor")
			}
			if len(gh.tree) != 0 {
				t.Errorf("something was committed: %v", gh.tree)
			}
			if entries, _ := os.ReadDir(dir); len(entries) > 1 {
				t.Errorf("files written: %v", entries)
			}
		})
	}
}

func TestReviewRequiredLeavesPullRequestOpen(t *testing.T) {
	gh := &fakeGitHub{mergeCode: http.StatusMethodNotAllowed}
	h, _ := newTestHandler(t, gh)
	code, out := post(t, h, &chief, validForm())
	if code != 200 || out["merged"] != false || out["pr"] != "https://github.com/prodeko/prodeko-org/pull/7" {
		t.Fatalf("status %d: %v", code, out)
	}
	if len(gh.deleted) != 0 {
		t.Errorf("branch of an open pull request deleted: %v", gh.deleted)
	}
}

// Changing only the title of an issue already in the archive needs no PDF
// and keeps the cover; uploading a cover of another format replaces the old
// file instead of leaving it behind.
func TestUpdateExisting(t *testing.T) {
	old := "year: 2026\nnumber: 4\ntitle: \"Vanha\"\npdf: 2026/proleko-2026-4.pdf\ncover: images/proleko/kannet/2026-4.webp\n"

	gh := &fakeGitHub{existing: map[string]string{"site/data/proleko/2026-4.yaml": old}}
	h, _ := newTestHandler(t, gh)
	f := validForm()
	f.pdf = nil
	if code, out := post(t, h, &chief, f); code != 200 {
		t.Fatalf("title only: %d %v", code, out)
	}
	files := gh.treePaths()
	if len(files) != 1 || !strings.Contains(string(files["site/data/proleko/2026-4.yaml"]),
		"cover: images/proleko/kannet/2026-4.webp") {
		t.Errorf("title-only change = %v", files)
	}

	gh = &fakeGitHub{existing: map[string]string{"site/data/proleko/2026-4.yaml": old}}
	h, _ = newTestHandler(t, gh)
	f = validForm()
	f.pdf = nil
	f.cover = []byte("\x89PNG\r\n\x1a\nnew-cover")
	if code, out := post(t, h, &chief, f); code != 200 {
		t.Fatalf("new cover: %d %v", code, out)
	}
	files = gh.treePaths()
	if v, ok := files["site/assets/images/proleko/kannet/2026-4.webp"]; !ok || v != nil {
		t.Errorf("old cover not removed: %v", files)
	}
	if string(files["site/assets/images/proleko/kannet/2026-4.png"]) != "\x89PNG\r\n\x1a\nnew-cover" {
		t.Errorf("new cover not written")
	}
}

// A data file is never trusted to name what gets deleted: a cover path
// outside the cover directory is left alone.
func TestOldCoverOutsideDirectoryIsNotRemoved(t *testing.T) {
	old := "year: 2026\nnumber: 4\ntitle: \"x\"\npdf: 2026/proleko-2026-4.pdf\ncover: images/../../../hugo.toml\n"
	gh := &fakeGitHub{existing: map[string]string{"site/data/proleko/2026-4.yaml": old}}
	h, _ := newTestHandler(t, gh)
	f := validForm()
	if code, out := post(t, h, &chief, f); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	for path, content := range gh.treePaths() {
		if content == nil {
			t.Errorf("removed %s", path)
		}
	}
}

func TestOnlyPost(t *testing.T) {
	gh := &fakeGitHub{}
	h, _ := newTestHandler(t, gh)
	req := httptest.NewRequest("GET", Path, nil)
	req = req.WithContext(session.NewContext(req.Context(), chief))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: %d", rec.Code)
	}
	_, _ = io.Copy(io.Discard, rec.Body)
}

func TestNewChecksDirectory(t *testing.T) {
	_, err := New(Config{
		Dir: filepath.Join(t.TempDir(), "missing"), EditorRoles: []string{"a"},
		Owner: "o", Repo: "r", Branch: "main", APIRoot: "http://x", Credential: ghauth.Static("t"),
	})
	if err == nil {
		t.Error("a missing directory was accepted")
	}
}
