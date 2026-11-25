// Package proleko stores the PDFs of Proleko?, the guild magazine.
//
// An issue is edited in Decap like everything else: a data file in
// site/data/proleko/ with the year, number and title. Only the PDF cannot go
// through Decap, because Decap commits every upload to git and a magazine is
// 5–100 MB; the build refuses files over 5 MB and GitHub over 100 MB. So the
// PDF field in Decap (site/static/admin/proleko-pdf.js) sends the file here,
// it is written to PROLEKO_DIR, which the web server serves at
// /media/proleko/, and the field stores the path this returns.
//
// The cover is the PDF's first page, rendered in the editor's browser and
// sent along as a JPEG; it is stored next to the PDF with the same name.
//
// Every file gets a name of its own, proleko-<year>-<number>-<random>.pdf,
// so a draft never overwrites the PDF a published issue links to. A PDF that
// is replaced, or whose draft is thrown away, stays on disk unlinked.
package proleko

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/prodeko/prodeko-org/proxy/internal/session"
)

// Limits. The largest issue in the archive is 102 MB; 200 MB leaves room for
// a print-quality file without inviting a disk to be filled by one request.
const (
	MaxPDFBytes   = 200 << 20
	MaxCoverBytes = 5 << 20
	maxFieldBytes = 64
	maxNumber     = 20

	// uploadDeadline is how long one upload may take. The server's own read
	// timeout is a minute, which a 100 MB file on a slow connection does not
	// fit in; this raises it for this route only.
	uploadDeadline = 20 * time.Minute
)

// Path is where the endpoint is mounted.
const Path = "/proleko/pdf"

// Handler serves POST /proleko/pdf. Mount it behind CORS, the session
// middleware and the editor role check, exactly like /github/*: anyone who
// may edit the site may upload, and nobody else.
type Handler struct {
	dir string
	log *slog.Logger
	now func() time.Time
}

// New checks that the directory exists and is writable, so a missing volume
// is reported when the server starts rather than when the first issue is due.
func New(dir string, log *slog.Logger) (*Handler, error) {
	if dir == "" || !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("proleko: the PDF directory must be an absolute path, got %q", dir)
	}
	if err := checkWritable(dir); err != nil {
		return nil, fmt.Errorf("proleko: %s is not usable for the PDFs: %w", dir, err)
	}
	if log == nil {
		log = slog.Default()
	}
	return &Handler{dir: dir, log: log, now: time.Now}, nil
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

// result is what the PDF field stores: paths below /media/proleko/.
type result struct {
	PDF   string `json:"pdf"`
	Cover string `json:"cover,omitempty"`
}

// userError is a refusal whose message is for the editor, in Finnish like
// the rest of the editing screen's labels.
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
	id, _ := session.FromContext(r.Context())

	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(uploadDeadline))
	_ = rc.SetWriteDeadline(time.Now().Add(uploadDeadline + time.Minute))
	r.Body = http.MaxBytesReader(w, r.Body, MaxPDFBytes+MaxCoverBytes+(1<<20))

	res, err := h.store(r)
	if err != nil {
		var ue *userError
		var mbe *http.MaxBytesError
		switch {
		case errors.As(err, &ue):
			writeJSON(w, ue.status, map[string]string{"error": ue.msg})
		case errors.As(err, &mbe):
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{
				"error": fmt.Sprintf("Tiedosto on liian suuri. PDF saa olla enintään %d Mt.", MaxPDFBytes>>20),
			})
		default:
			h.log.Error("proleko upload failed", "err", err, "sub", id.Subject)
			writeJSON(w, http.StatusInternalServerError, map[string]string{
				"error": "PDF:n tallennus epäonnistui. Yritä uudelleen, tai ota yhteyttä killan IT-tiimiin.",
			})
		}
		return
	}
	h.log.Info("proleko PDF stored", "pdf", res.PDF, "cover", res.Cover, "sub", id.Subject, "user", id.Username)
	writeJSON(w, http.StatusOK, res)
}

// store reads the form as it streams in. The PDF goes straight to a
// temporary file in the target directory, so a 100 MB issue is never held in
// memory and the final rename is atomic. The text fields have to come before
// the files, which is the order the PDF field sends them in.
func (h *Handler) store(r *http.Request) (result, error) {
	mr, err := r.MultipartReader()
	if err != nil {
		return result{}, refuse(http.StatusBadRequest, "Lomake puuttuu tai on väärää muotoa.")
	}

	var (
		year, number int
		pdfTemp      string
		cover        []byte
	)
	defer func() {
		if pdfTemp != "" {
			os.Remove(pdfTemp) // already gone once renamed into place
		}
	}()

	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return result{}, err
		}
		switch part.FormName() {
		case "year", "number":
			b, err := io.ReadAll(io.LimitReader(part, maxFieldBytes))
			if err != nil {
				return result{}, err
			}
			n, err := strconv.Atoi(strings.TrimSpace(string(b)))
			if err != nil {
				n = 0
			}
			if part.FormName() == "year" {
				year = n
			} else {
				number = n
			}
		case "pdf":
			if pdfTemp != "" {
				return result{}, refuse(http.StatusBadRequest, "Lähetä yksi PDF kerrallaan.")
			}
			if err := h.checkIssue(year, number); err != nil {
				return result{}, err
			}
			pdfTemp, err = h.receivePDF(part)
			if err != nil {
				return result{}, err
			}
		case "cover":
			if cover != nil {
				return result{}, refuse(http.StatusBadRequest, "Lähetä yksi kansikuva kerrallaan.")
			}
			b, err := io.ReadAll(io.LimitReader(part, MaxCoverBytes+1))
			if err != nil {
				return result{}, err
			}
			if len(b) > MaxCoverBytes {
				return result{}, refuse(http.StatusRequestEntityTooLarge, "Kansikuva saa olla enintään %d Mt.", MaxCoverBytes>>20)
			}
			if len(b) < 3 || b[0] != 0xFF || b[1] != 0xD8 || b[2] != 0xFF {
				return result{}, refuse(http.StatusBadRequest, "Kansikuvan pitää olla JPEG-kuva.")
			}
			cover = b
		default:
			return result{}, refuse(http.StatusBadRequest, "Tuntematon kenttä %q.", part.FormName())
		}
		part.Close()
	}
	if pdfTemp == "" {
		return result{}, refuse(http.StatusBadRequest, "PDF puuttuu.")
	}

	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		return result{}, err
	}
	base := fmt.Sprintf("%d/proleko-%d-%d-%s", year, year, number, hex.EncodeToString(suffix))
	final := filepath.Join(h.dir, filepath.FromSlash(base))
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return result{}, err
	}

	res := result{PDF: base + ".pdf"}
	// The cover first: a PDF in place without its cover would be a link the
	// page shows as a broken image.
	if cover != nil {
		if err := writeFile(final+".jpg", cover); err != nil {
			return result{}, err
		}
		res.Cover = base + ".jpg"
	}
	if err := os.Chmod(pdfTemp, 0o644); err != nil {
		return result{}, err
	}
	if err := os.Rename(pdfTemp, final+".pdf"); err != nil {
		return result{}, err
	}
	pdfTemp = ""
	return res, nil
}

// checkIssue validates the two numbers the file name is built from. Nothing
// else from the request becomes part of a path.
func (h *Handler) checkIssue(year, number int) error {
	thisYear := h.now().Year()
	if year < 1990 || year > thisYear+1 {
		return refuse(http.StatusBadRequest, "Täytä ensin vuosi (1990–%d).", thisYear+1)
	}
	if number < 1 || number > maxNumber {
		return refuse(http.StatusBadRequest, "Täytä ensin numero (1–%d).", maxNumber)
	}
	return nil
}

func (h *Handler) receivePDF(part *multipart.Part) (string, error) {
	incoming := filepath.Join(h.dir, ".incoming")
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
	fail := func(e error) (string, error) {
		os.Remove(path)
		return "", e
	}
	if err != nil {
		return fail(err)
	}
	if n > MaxPDFBytes {
		return fail(refuse(http.StatusRequestEntityTooLarge, "PDF saa olla enintään %d Mt.", MaxPDFBytes>>20))
	}
	head := make([]byte, 5)
	if rf, err := os.Open(path); err == nil {
		_, _ = io.ReadFull(rf, head)
		rf.Close()
	}
	if !bytes.Equal(head, []byte("%PDF-")) {
		return fail(refuse(http.StatusBadRequest, "Tiedosto ei ole PDF."))
	}
	return path, nil
}

func writeFile(path string, data []byte) error {
	tmp := path + ".part"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
