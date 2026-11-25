// Package proleko is the upload for Proleko?, the guild magazine: the one
// part of the website its editor-in-chief can change without being a website
// editor.
//
// An issue is three things. The PDF is written to PROLEKO_DIR, which the web
// server serves at /media/proleko/; at 1.3 GB for the archive, with single
// issues over GitHub's 100 MB limit, the PDFs cannot live in git. The cover
// and a small data file go to the repository, where the site build turns them
// into the archive page:
//
//	site/data/proleko/2026-3.yaml
//	site/assets/images/proleko/kannet/2026-3.jpg
//
// Both paths are built here from two validated integers. Nothing the browser
// sends becomes part of a path, so a person admitted by PROLEKO_ROLES can
// write these two files and the PDF, and nothing else: they never reach
// /github/*, which checks EditorRoles on every call.
//
// Without an uploaded cover the first page of the PDF is rendered with
// pdftoppm (poppler-utils). The commit goes through a pull request that is
// merged at once, or left open where branch protection wants a review.
package proleko

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/prodeko/prodeko-org/proxy/internal/ghauth"
	"github.com/prodeko/prodeko-org/proxy/internal/session"
)

// Limits. The largest issue in the archive is 102 MB; 200 MB leaves room for
// a print-quality file without inviting a disk to be filled by one request.
const (
	MaxPDFBytes   = 200 << 20
	MaxCoverBytes = 5 << 20
	maxFieldBytes = 1 << 10
	maxTitleRunes = 120
	maxNumber     = 20

	// uploadDeadline is how long one upload may take end to end. The
	// server's own read timeout is a minute, which a 100 MB file on a slow
	// connection does not fit in; this raises it for this route only.
	uploadDeadline = 20 * time.Minute
)

// Path is where the endpoint is mounted.
const Path = "/proleko/issues"

// Repository paths. The site's templates read the same places.
const (
	dataDir  = "site/data/proleko"
	coverDir = "site/assets/images/proleko/kannet"
)

// Config wires the handler. EditorRoles and ProlekoRoles are the same sets
// the sign-in uses; either one, held in full, admits a caller.
type Config struct {
	Dir          string
	EditorRoles  []string
	ProlekoRoles []string

	Owner, Repo, Branch, APIRoot string
	Credential                   ghauth.Source
	Committer                    Author

	// Render turns the first page of a PDF into a JPEG. Nil means pdftoppm,
	// when it is installed; with neither, a cover has to be uploaded.
	Render func(ctx context.Context, pdfPath string) ([]byte, error)

	HTTPClient *http.Client
	Now        func() time.Time
	Logger     *slog.Logger
}

// Handler serves POST /proleko/issues. Mount it behind CORS and
// session.Middleware; it checks the roles itself.
type Handler struct {
	cfg    Config
	gh     *github
	render func(ctx context.Context, pdfPath string) ([]byte, error)
	log    *slog.Logger
	now    func() time.Time

	// mu serialises uploads. Two at once would race for the same issue's
	// files, and one editor-in-chief uploads a few times a year.
	mu sync.Mutex
}

// New checks the configuration and that the PDF directory is writable, so a
// missing volume is reported when the server starts rather than when the
// first issue is due.
func New(cfg Config) (*Handler, error) {
	if cfg.Dir == "" || !filepath.IsAbs(cfg.Dir) {
		return nil, fmt.Errorf("proleko: Dir must be an absolute path, got %q", cfg.Dir)
	}
	if len(cfg.EditorRoles) == 0 {
		return nil, errors.New("proleko: EditorRoles must name at least one role")
	}
	if cfg.Credential == nil || cfg.Owner == "" || cfg.Repo == "" || cfg.Branch == "" || cfg.APIRoot == "" {
		return nil, errors.New("proleko: the GitHub repository and credential are required")
	}
	if err := checkWritable(cfg.Dir); err != nil {
		return nil, fmt.Errorf("proleko: %s is not usable for the PDFs: %w", cfg.Dir, err)
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 60 * time.Second}
	}
	h := &Handler{
		cfg:    cfg,
		render: cfg.Render,
		log:    cfg.Logger,
		now:    cfg.Now,
		gh: &github{
			api:        strings.TrimRight(cfg.APIRoot, "/"),
			owner:      cfg.Owner,
			repo:       cfg.Repo,
			branch:     cfg.Branch,
			credential: cfg.Credential,
			committer:  cfg.Committer,
			client:     cfg.HTTPClient,
			now:        cfg.Now,
		},
	}
	if h.render == nil {
		if bin, err := exec.LookPath("pdftoppm"); err == nil {
			h.render = pdftoppm(bin)
		} else {
			h.log.Warn("pdftoppm not found: a Proleko? upload must include a cover image")
		}
	}
	return h, nil
}

func checkWritable(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("not a directory")
	}
	f, err := os.CreateTemp(dir, ".write-check-*")
	if err != nil {
		return err
	}
	name := f.Name()
	f.Close()
	return os.Remove(name)
}

// allowed reports whether the identity may upload: every editor role, or
// every Proleko role when any are configured.
func (h *Handler) allowed(id session.Identity) bool {
	if len(id.MissingRoles(h.cfg.EditorRoles)) == 0 {
		return true
	}
	return len(h.cfg.ProlekoRoles) > 0 && len(id.MissingRoles(h.cfg.ProlekoRoles)) == 0
}

// issue is one validated upload.
type issue struct {
	year, number int
	title        string
	pdfTemp      string // a temporary file in Dir, or "" for a change to the title or cover only
	cover        []byte
	coverExt     string
}

func (i issue) slug() string    { return fmt.Sprintf("%d-%d", i.year, i.number) }
func (i issue) label() string   { return fmt.Sprintf("%d/%d", i.number, i.year) }
func (i issue) pdfPath() string { return fmt.Sprintf("%d/proleko-%s.pdf", i.year, i.slug()) }

// userError is a refusal whose message is for the person uploading. It is
// written in Finnish, the language of the upload page.
type userError struct {
	status int
	msg    string
}

func (e *userError) Error() string { return e.msg }

func refuse(status int, format string, args ...any) error {
	return &userError{status: status, msg: fmt.Sprintf(format, args...)}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "Vain POST."})
		return
	}
	id, ok := session.FromContext(r.Context())
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Kirjaudu sisään."})
		return
	}
	if !h.allowed(id) {
		h.log.Warn("proleko upload refused: missing roles", "sub", id.Subject, "user", id.Username)
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "Tunnukseltasi puuttuu oikeus lisätä Proleko?-lehtiä. Pyydä oikeutta killan IT-tiimiltä.",
		})
		return
	}

	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(uploadDeadline))
	_ = rc.SetWriteDeadline(time.Now().Add(uploadDeadline + time.Minute))
	r.Body = http.MaxBytesReader(w, r.Body, MaxPDFBytes+MaxCoverBytes+(1<<20))

	is, err := h.read(r)
	if is.pdfTemp != "" {
		defer os.Remove(is.pdfTemp) // gone already once renamed into place
	}
	if err != nil {
		h.fail(w, id, err)
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	res, err := h.save(r.Context(), id, is)
	if err != nil {
		h.fail(w, id, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (h *Handler) fail(w http.ResponseWriter, id session.Identity, err error) {
	var ue *userError
	if errors.As(err, &ue) {
		writeJSON(w, ue.status, map[string]string{"error": ue.msg})
		return
	}
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{
			"error": fmt.Sprintf("Tiedosto on liian suuri. PDF saa olla enintään %d Mt.", MaxPDFBytes>>20),
		})
		return
	}
	h.log.Error("proleko upload failed", "err", err, "sub", id.Subject)
	writeJSON(w, http.StatusBadGateway, map[string]string{
		"error": "Tallennus epäonnistui: " + err.Error() + ". Yritä uudelleen, tai ota yhteyttä killan IT-tiimiin.",
	})
}

// read takes the multipart form apart as it streams in: the PDF goes straight
// to a temporary file next to its final place, so a 100 MB issue is never
// held in memory, and the rename at the end is atomic.
func (h *Handler) read(r *http.Request) (issue, error) {
	var is issue
	mr, err := r.MultipartReader()
	if err != nil {
		return is, refuse(http.StatusBadRequest, "Lomake puuttuu tai on väärää muotoa.")
	}
	fields := map[string]string{}
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return is, err
		}
		name := part.FormName()
		switch name {
		case "year", "number", "title":
			if _, dup := fields[name]; dup {
				return is, refuse(http.StatusBadRequest, "Kenttä %s on lomakkeessa kahdesti.", name)
			}
			b, err := io.ReadAll(io.LimitReader(part, maxFieldBytes+1))
			if err != nil {
				return is, err
			}
			if len(b) > maxFieldBytes {
				return is, refuse(http.StatusBadRequest, "Kenttä %s on liian pitkä.", name)
			}
			fields[name] = string(b)
		case "pdf":
			if is.pdfTemp != "" {
				return is, refuse(http.StatusBadRequest, "Lähetä yksi PDF kerrallaan.")
			}
			if part.FileName() == "" {
				continue // the empty file input of a form sent without a PDF
			}
			path, err := h.receivePDF(part)
			is.pdfTemp = path
			if err != nil {
				return is, err
			}
		case "cover":
			if is.cover != nil {
				return is, refuse(http.StatusBadRequest, "Lähetä yksi kansikuva kerrallaan.")
			}
			if part.FileName() == "" {
				continue
			}
			b, err := io.ReadAll(io.LimitReader(part, MaxCoverBytes+1))
			if err != nil {
				return is, err
			}
			if len(b) > MaxCoverBytes {
				return is, refuse(http.StatusRequestEntityTooLarge, "Kansikuva saa olla enintään %d Mt.", MaxCoverBytes>>20)
			}
			ext := imageExt(b)
			if ext == "" {
				return is, refuse(http.StatusBadRequest, "Kansikuvan pitää olla JPEG-, PNG- tai WebP-kuva.")
			}
			is.cover, is.coverExt = b, ext
		default:
			return is, refuse(http.StatusBadRequest, "Tuntematon kenttä %q.", name)
		}
		part.Close()
	}

	year, err := strconv.Atoi(strings.TrimSpace(fields["year"]))
	thisYear := h.now().Year()
	if err != nil || year < 1990 || year > thisYear+1 {
		return is, refuse(http.StatusBadRequest, "Vuoden pitää olla väliltä 1990–%d.", thisYear+1)
	}
	number, err := strconv.Atoi(strings.TrimSpace(fields["number"]))
	if err != nil || number < 1 || number > maxNumber {
		return is, refuse(http.StatusBadRequest, "Numeron pitää olla väliltä 1–%d.", maxNumber)
	}
	title, err := cleanTitle(fields["title"])
	if err != nil {
		return is, err
	}
	is.year, is.number, is.title = year, number, title
	return is, nil
}

func (h *Handler) receivePDF(part *multipart.Part) (string, error) {
	incoming := filepath.Join(h.cfg.Dir, ".incoming")
	if err := os.MkdirAll(incoming, 0o750); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(incoming, "upload-*.pdf")
	if err != nil {
		return "", err
	}
	path := f.Name()
	n, err := io.Copy(f, io.LimitReader(part, MaxPDFBytes+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return path, err
	}
	if n > MaxPDFBytes {
		return path, refuse(http.StatusRequestEntityTooLarge, "PDF saa olla enintään %d Mt.", MaxPDFBytes>>20)
	}
	head := make([]byte, 5)
	if rf, err := os.Open(path); err == nil {
		_, _ = io.ReadFull(rf, head)
		rf.Close()
	}
	if !bytes.Equal(head, []byte("%PDF-")) {
		return path, refuse(http.StatusBadRequest, "Tiedosto ei ole PDF.")
	}
	return path, nil
}

// cleanTitle allows an empty title (the old archive has many untitled
// issues) but no control characters, since the title goes into YAML and a
// commit message.
func cleanTitle(raw string) (string, error) {
	t := strings.TrimSpace(raw)
	if !utf8.ValidString(t) {
		return "", refuse(http.StatusBadRequest, "Nimessä on virheellisiä merkkejä.")
	}
	for _, r := range t {
		if unicode.IsControl(r) {
			return "", refuse(http.StatusBadRequest, "Nimessä ei voi olla rivinvaihtoja tai ohjausmerkkejä.")
		}
	}
	if utf8.RuneCountInString(t) > maxTitleRunes {
		return "", refuse(http.StatusBadRequest, "Nimi saa olla enintään %d merkkiä.", maxTitleRunes)
	}
	return t, nil
}

// imageExt sniffs the three formats Hugo can resize, by their magic bytes
// rather than by what the browser claims.
func imageExt(b []byte) string {
	switch {
	case len(b) > 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return "jpg"
	case len(b) > 8 && bytes.Equal(b[:8], []byte("\x89PNG\r\n\x1a\n")):
		return "png"
	case len(b) > 12 && bytes.Equal(b[:4], []byte("RIFF")) && bytes.Equal(b[8:12], []byte("WEBP")):
		return "webp"
	}
	return ""
}

// result is what the upload page is told.
type result struct {
	Slug    string `json:"slug"`
	PDF     string `json:"pdf"`
	Merged  bool   `json:"merged"`
	PRURL   string `json:"pr,omitempty"`
	Message string `json:"message"`
}

func (h *Handler) save(ctx context.Context, id session.Identity, is issue) (result, error) {
	dataPath := fmt.Sprintf("%s/%s.yaml", dataDir, is.slug())
	existing, err := h.gh.read(ctx, dataPath)
	if err != nil {
		return result{}, fmt.Errorf("reading %s: %w", dataPath, err)
	}
	oldCover := yamlField(existing, "cover")
	if existing == nil && is.pdfTemp == "" {
		return result{}, refuse(http.StatusBadRequest, "Numeroa %s ei ole vielä arkistossa, joten PDF tarvitaan.", is.label())
	}

	if is.cover == nil && is.pdfTemp != "" && h.render != nil {
		img, err := h.render(ctx, is.pdfTemp)
		if err != nil {
			h.log.Warn("could not render a cover from the PDF", "err", err)
		} else {
			is.cover, is.coverExt = img, "jpg"
		}
	}
	if is.cover == nil && oldCover == "" {
		return result{}, refuse(http.StatusBadRequest, "Kansikuvaa ei saatu tehtyä PDF:stä. Lisää kansikuva erikseen.")
	}

	ch := change{files: map[string][]byte{}}
	cover := oldCover
	if is.cover != nil {
		cover = fmt.Sprintf("images/proleko/kannet/%s.%s", is.slug(), is.coverExt)
		ch.files[fmt.Sprintf("%s/%s.%s", coverDir, is.slug(), is.coverExt)] = is.cover
		// A new cover in another format leaves the old file behind unless it
		// is removed in the same commit. Only a path inside the cover
		// directory is ever removed, whatever the data file said.
		if oldCover != "" && oldCover != cover {
			old := "site/assets/" + oldCover
			if strings.HasPrefix(old, coverDir+"/") && !strings.Contains(old[len(coverDir)+1:], "/") &&
				!strings.Contains(old, "..") {
				ch.removed = append(ch.removed, old)
			}
		}
	}
	ch.files[dataPath] = issueYAML(is, cover)

	name := is.title
	if name == "" {
		name = "nimetön numero"
	}
	title := fmt.Sprintf("Proleko? %s: %s", is.label(), name)
	body := fmt.Sprintf("%s lisäsi numeron %s Proleko?-arkistoon lataussivulta.", id.Name, is.label())
	if existing != nil {
		body = fmt.Sprintf("%s päivitti numeron %s Proleko?-arkistossa lataussivulta.", id.Name, is.label())
	}
	if is.pdfTemp != "" {
		body += fmt.Sprintf("\n\nPDF: /media/proleko/%s (palvelimen levyllä, ei gitissä).", is.pdfPath())
	}
	branch := fmt.Sprintf("proleko/%s-%d", is.slug(), h.now().Unix())

	// The PDF is put in place before the commit. If the commit then fails,
	// the PDF sits at an address nothing links to yet and the next attempt
	// overwrites it; the other order could publish a link to a missing file.
	if is.pdfTemp != "" {
		final := filepath.Join(h.cfg.Dir, filepath.FromSlash(is.pdfPath()))
		if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
			return result{}, err
		}
		if err := os.Chmod(is.pdfTemp, 0o644); err != nil {
			return result{}, err
		}
		if err := os.Rename(is.pdfTemp, final); err != nil {
			return result{}, err
		}
	}

	pub, err := h.gh.publish(ctx, ch, Author{Name: id.Name, Email: id.Email}, branch, title, body)
	if err != nil {
		return result{}, err
	}
	h.log.Info("proleko issue saved", "issue", is.slug(), "sub", id.Subject, "merged", pub.Merged, "pr", pub.PRURL)

	res := result{Slug: is.slug(), PDF: "/media/proleko/" + is.pdfPath(), Merged: pub.Merged, PRURL: pub.PRURL}
	if pub.Merged {
		res.Message = fmt.Sprintf("Proleko? %s on tallennettu. Se näkyy sivuilla muutaman minuutin kuluttua, kun sivusto on päivittynyt.", is.label())
	} else {
		res.Message = fmt.Sprintf("Proleko? %s on tallennettu ja odottaa hyväksyntää. Sivuston ylläpitäjä hyväksyy muutoksen GitHubissa.", is.label())
	}
	return res, nil
}

// issueYAML writes the data file in the shape tools/import-proleko.py writes
// for the old archive. The title is the only free text, and it is quoted with
// JSON's escaping, which YAML reads as a double-quoted scalar.
func issueYAML(is issue, cover string) []byte {
	q, _ := json.Marshal(is.title)
	return []byte(fmt.Sprintf("year: %d\nnumber: %d\ntitle: %s\npdf: %s\ncover: %s\n",
		is.year, is.number, q, is.pdfPath(), cover))
}

// yamlField reads one top-level "key: value" line of a data file written by
// issueYAML or the importer. Those are the only writers, so a full YAML
// parser would only add a dependency.
func yamlField(doc []byte, key string) string {
	for _, line := range strings.Split(string(doc), "\n") {
		if v, ok := strings.CutPrefix(line, key+":"); ok {
			return strings.Trim(strings.TrimSpace(v), `"`)
		}
	}
	return ""
}

// pdftoppm renders page one at 1000 px wide as a JPEG.
func pdftoppm(bin string) func(ctx context.Context, pdfPath string) ([]byte, error) {
	return func(ctx context.Context, pdfPath string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		dir, err := os.MkdirTemp("", "proleko-cover-")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(dir)
		out := filepath.Join(dir, "cover")
		cmd := exec.CommandContext(ctx, bin, "-f", "1", "-l", "1", "-singlefile",
			"-jpeg", "-jpegopt", "quality=85", "-scale-to-x", "1000", "-scale-to-y", "-1", pdfPath, out)
		if msg, err := cmd.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("pdftoppm: %v: %s", err, strings.TrimSpace(string(msg)))
		}
		return os.ReadFile(out + ".jpg")
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
