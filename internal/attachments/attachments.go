// Package attachments moves message files between the ex server and the
// local disk for one run: the invoking message's files land in a per-run
// temp dir before the harness starts, fetch_attachment pulls older thread
// files into the same dir, and the post tools upload files the agent wants
// to share. Names and bytes come from other people, so every filename is
// rebuilt here and every declared image/PDF is checked against its magic
// bytes before any tool is told what it is.
package attachments

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode"

	"github.com/DigitalTolk/ex-runners/internal/api"
	"github.com/DigitalTolk/ex-runners/internal/protocol"
)

const (
	// DownloadMax caps one downloaded file. The server's own upload cap is
	// lower; this is the runner's guard against a server that is not ours.
	DownloadMax int64 = 25 << 20
	// UploadMax is the server's per-file cap, refused locally before any
	// bytes move.
	UploadMax int64 = 10 << 20
	// MaxPerRun bounds how many invoking-message files land on disk.
	MaxPerRun = 10
	// MaxPerPost is how many attachments one post may carry.
	MaxPerPost = 5
	// stemMax clips an untrusted filename's stem.
	stemMax = 100
)

// uploadTypes is the server's extension allow-list with the MIME type each
// is sent as. A table, not mime.TypeByExtension: that reads the machine's
// mime database and answers differently from one OS to the next.
var uploadTypes = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif", ".webp": "image/webp",
	".pdf": "application/pdf", ".txt": "text/plain", ".md": "text/markdown", ".csv": "text/csv",
	".json": "application/json", ".html": "text/html",
	".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
}

// AllowedExtensions lists the upload allow-list for error messages.
const AllowedExtensions = "png jpg jpeg gif webp pdf txt md csv json html docx xlsx pptx"

// UploadType is the MIME type a file is uploaded as — "" when its extension
// is not on the server's allow-list.
func UploadType(path string) string { return uploadTypes[strings.ToLower(filepath.Ext(path))] }

// SafeName rebuilds an untrusted filename as something safe to create under
// the attachments dir: path separators and ".." become "_", control
// characters and leading dots go, the stem is clipped, and the extension is
// kept only when it is short and alphanumeric (so "x.png/../../y" or
// "a.sh\x00.png" cannot smuggle anything past the directory).
func SafeName(name string) string {
	name = strings.NewReplacer("/", "_", `\`, "_", ":", "_", "..", "_").Replace(name)
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == unicode.ReplacementChar {
			return -1
		}
		return r
	}, name)
	name = strings.TrimLeft(name, ". ")
	ext := filepath.Ext(name)
	if !plainExt(ext) {
		ext = ""
	}
	stem := strings.TrimSuffix(name, ext)
	if runes := []rune(stem); len(runes) > stemMax {
		stem = string(runes[:stemMax])
	}
	stem = strings.TrimRight(stem, ". ")
	if stem == "" {
		stem = "file"
	}
	return stem + ext
}

// plainExt: ".ext" of 1–8 ASCII letters/digits.
func plainExt(ext string) bool {
	if len(ext) < 2 || len(ext) > 9 {
		return false
	}
	for _, r := range ext[1:] {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// matchesMagic checks the leading bytes of the formats the runner verifies.
// Office/zip formats are never parsed: unzipping untrusted archives is a
// bigger risk than the check would remove. Other types are taken as declared.
func matchesMagic(contentType string, head []byte) bool {
	switch contentType {
	case "image/png":
		return bytes.HasPrefix(head, []byte("\x89PNG\r\n\x1a\n"))
	case "image/jpeg":
		return bytes.HasPrefix(head, []byte{0xFF, 0xD8, 0xFF})
	case "image/gif":
		return bytes.HasPrefix(head, []byte("GIF87a")) || bytes.HasPrefix(head, []byte("GIF89a"))
	case "image/webp":
		return len(head) >= 12 && string(head[:4]) == "RIFF" && string(head[8:12]) == "WEBP"
	case "application/pdf":
		return bytes.HasPrefix(head, []byte("%PDF-"))
	}
	return true
}

// Store is one run's attachments directory. Files are numbered in arrival
// order and a number is never reused, so two files both called photo.png
// coexist. The runner fills it first; the MCP server continues the count.
type Store struct {
	Dir string

	mu sync.Mutex
	n  int
}

// NewStore opens dir, continuing the numbering after whatever is there.
func NewStore(dir string) *Store {
	entries, _ := os.ReadDir(dir) // unreadable → start at 01; O_EXCL still keeps names unique
	return &Store{Dir: dir, n: len(entries)}
}

// Saved describes one file written into the store.
type Saved struct {
	Path, Name, ContentType string
	Size                    int64
	// Mismatch: the bytes do not match the declared image/PDF type. The file
	// is kept, but as .bin so no tool treats it as that type.
	Mismatch bool
}

// Save writes r into the store under a fresh numbered name built from
// filename, refusing streams longer than max bytes.
func (s *Store) Save(r io.Reader, filename, contentType string, max int64) (Saved, error) {
	head := make([]byte, 12)
	n, err := io.ReadFull(r, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return Saved{}, err
	}
	head = head[:n]
	name := SafeName(filename)
	mismatch := !matchesMagic(contentType, head)
	if mismatch {
		name = strings.TrimSuffix(name, filepath.Ext(name)) + ".bin"
	}
	f, err := s.create(name)
	if err != nil {
		return Saved{}, err
	}
	size, err := writeCapped(f, head, r, max)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(f.Name())
		return Saved{}, err
	}
	return Saved{Path: f.Name(), Name: filepath.Base(f.Name()), ContentType: contentType, Size: size, Mismatch: mismatch}, nil
}

// create opens NN-name with the next number, owner-only. The name is
// created exclusively: should it already exist (another process numbered
// it first), the next number is tried rather than overwriting.
func (s *Store) create(name string) (*os.File, error) {
	for {
		s.mu.Lock()
		s.n++
		n := s.n
		s.mu.Unlock()
		f, err := os.OpenFile(filepath.Join(s.Dir, fmt.Sprintf("%02d-%s", n, name)), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
	}
}

// writeCapped writes head then the rest of r, stopping one byte past max
// so an over-long stream is detected without buffering it.
func writeCapped(f *os.File, head []byte, r io.Reader, max int64) (int64, error) {
	size, err := io.Copy(f, io.MultiReader(bytes.NewReader(head), io.LimitReader(r, max+1-int64(len(head)))))
	if err != nil {
		return 0, err
	}
	if size > max {
		return 0, fmt.Errorf("larger than %s", SizeLabel(max))
	}
	return size, nil
}

// StatusError is a non-2xx answer from the server: the status plus its
// flattened {error, message} body, for the same failure wording the other
// tools use.
type StatusError struct {
	Status int
	Data   map[string]any
}

func (e *StatusError) Error() string {
	if msg, ok := e.Data["message"].(string); ok && msg != "" {
		return fmt.Sprintf("HTTP %d: %s", e.Status, msg)
	}
	return fmt.Sprintf("HTTP %d", e.Status)
}

// Download fetches one attachment through the run's base URL — never a URL
// found in a payload — and saves it. meta's filename and type are used when
// the claim supplied them; otherwise the response headers name the file.
func Download(ctx context.Context, client *http.Client, baseURL, runToken string, store *Store, meta protocol.Attachment) (Saved, error) {
	u := baseURL + "/api/v1/agent/run/attachments/" + url.PathEscape(meta.ID)
	if meta.MessageID != "" {
		u += "?messageID=" + url.QueryEscape(meta.MessageID)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Saved{}, err
	}
	req.Header.Set("Authorization", "Bearer "+runToken)
	res, err := client.Do(req)
	if err != nil {
		return Saved{}, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return Saved{}, statusError(res)
	}
	if res.ContentLength > DownloadMax {
		return Saved{}, fmt.Errorf("larger than %s", SizeLabel(DownloadMax))
	}
	name, ctype := meta.Filename, meta.ContentType
	if name == "" {
		_, params, _ := mime.ParseMediaType(res.Header.Get("Content-Disposition"))
		name = params["filename"]
	}
	if name == "" {
		name = meta.ID
	}
	if ctype == "" {
		ctype, _, _ = mime.ParseMediaType(res.Header.Get("Content-Type"))
	}
	return store.Save(res.Body, name, ctype, DownloadMax)
}

// Upload streams one local file to the run's upload endpoint and returns
// the attachment id. The caller has vetted WHERE the file is (package mcp's
// share policy); this checks what the server will refuse anyway — type,
// size — before sending a byte. Content-Length is set from the file size:
// the server rejects chunked bodies.
func Upload(ctx context.Context, client *http.Client, baseURL, runToken, path string) (string, error) {
	ctype := UploadType(path)
	if ctype == "" {
		return "", fmt.Errorf("file type not allowed (allowed: %s)", AllowedExtensions)
	}
	st, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	switch {
	case st.Size() == 0:
		return "", errors.New("file is empty")
	case st.Size() > UploadMax:
		return "", fmt.Errorf("larger than %s", SizeLabel(UploadMax))
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/v1/agent/run/attachments", f)
	if err != nil {
		return "", err
	}
	req.ContentLength = st.Size()
	req.Header.Set("Authorization", "Bearer "+runToken)
	req.Header.Set("Content-Type", ctype)
	req.Header.Set("X-Ex-Filename", url.PathEscape(filepath.Base(path)))
	res, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return "", statusError(res)
	}
	var body struct {
		Attachment struct {
			ID string `json:"id"`
		} `json:"attachment"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&body); err != nil || body.Attachment.ID == "" {
		return "", errors.New("the server returned no attachment id")
	}
	return body.Attachment.ID, nil
}

// statusError reads a rejection body (bounded) into a StatusError.
func statusError(res *http.Response) *StatusError {
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	data := map[string]any{}
	_ = json.Unmarshal(raw, &data) // non-JSON → {}
	if data == nil {
		data = map[string]any{}
	}
	return &StatusError{Status: res.StatusCode, Data: api.FlattenBackendError(data)}
}

// SizeLabel renders a byte count for humans and prompts: "812 B", "120 KB",
// "1.5 MB".
func SizeLabel(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/float64(1<<20))
	case n >= 1024:
		return fmt.Sprintf("%d KB", (n+512)/1024)
	}
	return fmt.Sprintf("%d B", n)
}
