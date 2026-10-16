package attachments

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DigitalTolk/ex-runners/internal/protocol"
)

const pngHead = "\x89PNG\r\n\x1a\n" + "rest-of-image"

func TestSafeName(t *testing.T) {
	cases := map[string]string{
		"photo.png":                         "photo.png",
		"../../etc/passwd":                  "____etc_passwd",
		`C:\Users\me\notes.txt`:             "C__Users_me_notes.txt",
		".hidden":                           "hidden",
		"...":                               "_",
		"":                                  "file",
		"a\x00b\n.png":                      "ab.png",
		"report.tar.gz":                     "report.tar.gz",
		"weird.ex-t":                        "weird.ex-t",
		"toolong." + strings.Repeat("x", 9): "toolong." + strings.Repeat("x", 9),
		strings.Repeat("a", 120) + ".pdf":   strings.Repeat("a", 100) + ".pdf",
		"trailing. .png":                    "trailing.png",
		"bad\xffbytes.jpg":                  "badbytes.jpg",
	}
	for in, want := range cases {
		if got := SafeName(in); got != want {
			t.Errorf("SafeName(%q) = %q, want %q", in, got, want)
		}
	}
	if !plainExt(".PnG9") || plainExt(".") || plainExt(".toolongext") || plainExt(".a-b") {
		t.Error("plainExt")
	}
}

func TestUploadTypeAndSizeLabel(t *testing.T) {
	if UploadType("/x/a.PNG") != "image/png" || UploadType("a.docx") == "" || UploadType("a.sh") != "" || UploadType("noext") != "" {
		t.Error("UploadType")
	}
	for n, want := range map[int64]string{12: "12 B", 1024: "1 KB", 122880: "120 KB", 1536: "2 KB", 1 << 20: "1.0 MB", 25 << 20: "25.0 MB"} {
		if got := SizeLabel(n); got != want {
			t.Errorf("SizeLabel(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestStoreSaveNamesSniffsAndCaps(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)
	good, err := st.Save(strings.NewReader(pngHead), "../photo.png", "image/png", DownloadMax)
	if err != nil || good.Name != "01-__photo.png" || good.Size != int64(len(pngHead)) || good.Mismatch || good.Path != filepath.Join(dir, good.Name) {
		t.Fatalf("saved = %+v err=%v", good, err)
	}
	if info, _ := os.Stat(good.Path); info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", info.Mode())
	}
	// Declared PNG, actually a script: kept, but as .bin.
	bad, err := st.Save(strings.NewReader("#!/bin/sh\necho pwned"), "photo.png", "image/png", DownloadMax)
	if err != nil || bad.Name != "02-photo.bin" || !bad.Mismatch {
		t.Errorf("mismatch = %+v err=%v", bad, err)
	}
	// Every sniffed format, matching and not.
	for ctype, head := range map[string]string{
		"image/jpeg": "\xFF\xD8\xFFdata", "image/gif": "GIF89a...", "image/webp": "RIFF\x00\x00\x00\x00WEBPVP8 ", "application/pdf": "%PDF-1.7",
	} {
		if s, _ := st.Save(strings.NewReader(head), "f.x", ctype, DownloadMax); s.Mismatch {
			t.Errorf("%s: real bytes flagged", ctype)
		}
		if s, _ := st.Save(strings.NewReader("nope nope nope"), "f.x", ctype, DownloadMax); !s.Mismatch {
			t.Errorf("%s: wrong bytes accepted", ctype)
		}
	}
	if s, _ := st.Save(strings.NewReader("GIF87a"), "f", "image/gif", DownloadMax); s.Mismatch {
		t.Error("GIF87a flagged")
	}
	// Unsniffed types are taken as declared; an empty stream is a file too.
	if s, err := st.Save(strings.NewReader(""), "notes.txt", "text/plain", DownloadMax); err != nil || s.Mismatch || s.Size != 0 {
		t.Errorf("empty = %+v err=%v", s, err)
	}
	// Over the cap: refused and not left behind.
	before, _ := os.ReadDir(dir)
	if _, err := st.Save(strings.NewReader(strings.Repeat("x", 40)), "big.txt", "text/plain", 30); err == nil || !strings.Contains(err.Error(), "larger than 30 B") {
		t.Errorf("cap err = %v", err)
	}
	if _, err := st.Save(strings.NewReader("0123456789abcdef"), "tiny.txt", "text/plain", 4); err == nil {
		t.Error("a head longer than the cap must be refused")
	}
	if after, _ := os.ReadDir(dir); len(after) != len(before) {
		t.Error("refused file left on disk")
	}
	// A stream that fails mid-way.
	if _, err := st.Save(io.MultiReader(strings.NewReader("0123456789abcdef"), failingReader{}), "x.txt", "text/plain", DownloadMax); err == nil || err.Error() != "boom" {
		t.Errorf("copy err = %v", err)
	}
	if _, err := st.Save(failingReader{}, "x.txt", "text/plain", DownloadMax); err == nil || err.Error() != "boom" {
		t.Errorf("head err = %v", err)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("boom") }

func TestStoreNumberingContinuesAndNeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "01-a.txt"), []byte("first"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "02-b.txt"), []byte("second"), 0o600)
	// A new store continues after what is there (the MCP server after the runner).
	if s, _ := NewStore(dir).Save(strings.NewReader("x"), "a.txt", "text/plain", DownloadMax); s.Name != "03-a.txt" {
		t.Errorf("continued name = %s", s.Name)
	}
	// A store that lost count skips names that already exist instead of overwriting.
	s, err := (&Store{Dir: dir}).Save(strings.NewReader("y"), "a.txt", "text/plain", DownloadMax)
	if err != nil || s.Name != "02-a.txt" {
		t.Errorf("dedupe = %+v err=%v", s, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "01-a.txt")); string(b) != "first" {
		t.Error("existing file overwritten")
	}
	// Missing dir: the create error surfaces.
	if _, err := NewStore(filepath.Join(dir, "missing")).Save(strings.NewReader("x"), "a.txt", "text/plain", DownloadMax); err == nil {
		t.Error("missing dir must fail")
	}
}

// fileServer answers GET /api/v1/agent/run/attachments/{id} and POST
// /api/v1/agent/run/attachments, recording the last request.
type fileServer struct {
	srv  *httptest.Server
	last *http.Request
	body []byte
	// per-id download answers; status 0 = 200 with the body
	files map[string]struct {
		status  int
		body    string
		headers map[string]string
	}
	upload func(w http.ResponseWriter)
}

func newFileServer(t *testing.T) *fileServer {
	f := &fileServer{files: map[string]struct {
		status  int
		body    string
		headers map[string]string
	}{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.last = r
		f.body, _ = io.ReadAll(r.Body)
		if r.Method == http.MethodPost {
			f.upload(w)
			return
		}
		a, ok := f.files[strings.TrimPrefix(r.URL.Path, "/api/v1/agent/run/attachments/")]
		if !ok {
			w.WriteHeader(404)
			_, _ = io.WriteString(w, `{"error":{"code":"not_found","message":"no such file"}}`)
			return
		}
		for k, v := range a.headers {
			w.Header().Set(k, v)
		}
		if a.status != 0 {
			w.WriteHeader(a.status)
		}
		_, _ = io.WriteString(w, a.body)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func TestDownload(t *testing.T) {
	f := newFileServer(t)
	f.files["att1"] = struct {
		status  int
		body    string
		headers map[string]string
	}{body: pngHead, headers: map[string]string{"Content-Type": "image/png", "Content-Disposition": `attachment; filename="from header.png"`}}
	f.files["big"] = struct {
		status  int
		body    string
		headers map[string]string
	}{body: "x", headers: map[string]string{"Content-Length": "99999999"}}
	f.files["noname"] = struct {
		status  int
		body    string
		headers map[string]string
	}{body: "hello", headers: map[string]string{"Content-Type": "text/plain; charset=utf-8"}}
	f.files["plain404"] = struct {
		status  int
		body    string
		headers map[string]string
	}{status: 404, body: "not json"}
	f.files["null404"] = struct {
		status  int
		body    string
		headers map[string]string
	}{status: 404, body: "null"}
	ctx := context.Background()
	store := NewStore(t.TempDir())

	// Claim metadata wins over the headers; the request carries the token and the messageID.
	s, err := Download(ctx, http.DefaultClient, f.srv.URL, "tok", store, protocol.Attachment{ID: "att1", MessageID: "m 1", Filename: "photo.png", ContentType: "image/png"})
	if err != nil || s.Name != "01-photo.png" || s.ContentType != "image/png" || s.Size != int64(len(pngHead)) {
		t.Fatalf("saved = %+v err=%v", s, err)
	}
	if f.last.Header.Get("Authorization") != "Bearer tok" || f.last.URL.RawQuery != "messageID=m+1" {
		t.Errorf("request = %s %s", f.last.Header.Get("Authorization"), f.last.URL.String())
	}
	// Without metadata the headers name the file; without a header, the id does.
	if s, err = Download(ctx, http.DefaultClient, f.srv.URL, "tok", store, protocol.Attachment{ID: "att1"}); err != nil || s.Name != "02-from header.png" || s.ContentType != "image/png" || f.last.URL.RawQuery != "" {
		t.Errorf("header-named = %+v err=%v query=%q", s, err, f.last.URL.RawQuery)
	}
	if s, err = Download(ctx, http.DefaultClient, f.srv.URL, "tok", store, protocol.Attachment{ID: "noname"}); err != nil || s.Name != "03-noname" || s.ContentType != "text/plain" {
		t.Errorf("id-named = %+v err=%v", s, err)
	}
	// Server rejections carry the status and the flattened body; a non-JSON body still reads.
	var se *StatusError
	if _, err = Download(ctx, http.DefaultClient, f.srv.URL, "tok", store, protocol.Attachment{ID: "nope"}); !errors.As(err, &se) || se.Status != 404 || se.Data["error"] != "not_found" || err.Error() != "HTTP 404: no such file" {
		t.Errorf("404 err = %v", err)
	}
	if _, err = Download(ctx, http.DefaultClient, f.srv.URL, "tok", store, protocol.Attachment{ID: "plain404"}); !errors.As(err, &se) || err.Error() != "HTTP 404" {
		t.Errorf("plain 404 err = %v", err)
	}
	if _, err = Download(ctx, http.DefaultClient, f.srv.URL, "tok", store, protocol.Attachment{ID: "null404"}); !errors.As(err, &se) || err.Error() != "HTTP 404" || se.Data == nil {
		t.Errorf("null 404 err = %v", err)
	}
	// An announced oversize body is refused before reading it.
	if _, err = Download(ctx, http.DefaultClient, f.srv.URL, "tok", store, protocol.Attachment{ID: "big"}); err == nil || !strings.Contains(err.Error(), "larger than 25.0 MB") {
		t.Errorf("big err = %v", err)
	}
	// Transport and request-building failures.
	if _, err = Download(ctx, http.DefaultClient, "http://127.0.0.1:1", "tok", store, protocol.Attachment{ID: "x"}); err == nil {
		t.Error("transport error swallowed")
	}
	if _, err = Download(ctx, http.DefaultClient, "::bad url", "tok", store, protocol.Attachment{ID: "x"}); err == nil {
		t.Error("bad url swallowed")
	}
}

func TestUpload(t *testing.T) {
	f := newFileServer(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "my chart.png")
	_ = os.WriteFile(file, []byte(pngHead), 0o600)
	ctx := context.Background()
	f.upload = func(w http.ResponseWriter) {
		w.WriteHeader(201)
		_, _ = io.WriteString(w, `{"attachment":{"id":"att9","filename":"my chart.png","contentType":"image/png","size":21}}`)
	}
	id, err := Upload(ctx, http.DefaultClient, f.srv.URL, "tok", file)
	if err != nil || id != "att9" {
		t.Fatalf("id=%q err=%v", id, err)
	}
	h := f.last.Header
	if h.Get("Authorization") != "Bearer tok" || h.Get("Content-Type") != "image/png" || h.Get("X-Ex-Filename") != "my%20chart.png" || f.last.ContentLength != int64(len(pngHead)) || string(f.body) != pngHead {
		t.Errorf("request = %v len=%d body=%q", h, f.last.ContentLength, f.body)
	}
	if len(f.last.TransferEncoding) != 0 {
		t.Errorf("chunked upload: %v", f.last.TransferEncoding)
	}
	// Server rejections and malformed success bodies.
	f.upload = func(w http.ResponseWriter) {
		w.WriteHeader(413)
		_, _ = io.WriteString(w, `{"error":{"code":"too_large","message":"max 10 MiB"}}`)
	}
	var se *StatusError
	if _, err = Upload(ctx, http.DefaultClient, f.srv.URL, "tok", file); !errors.As(err, &se) || se.Status != 413 || se.Data["message"] != "max 10 MiB" {
		t.Errorf("413 err = %v", err)
	}
	for _, body := range []string{`{}`, `not json`, `null`} {
		f.upload = func(w http.ResponseWriter) { w.WriteHeader(201); _, _ = io.WriteString(w, body) }
		if _, err = Upload(ctx, http.DefaultClient, f.srv.URL, "tok", file); err == nil || err.Error() != "the server returned no attachment id" {
			t.Errorf("body %q: err = %v", body, err)
		}
	}
	// Local refusals happen before any request.
	f.last = nil
	empty := filepath.Join(dir, "empty.txt")
	_ = os.WriteFile(empty, nil, 0o600)
	big := filepath.Join(dir, "big.txt")
	_ = os.WriteFile(big, make([]byte, UploadMax+1), 0o600)
	for path, want := range map[string]string{
		filepath.Join(dir, "run.sh"):   "file type not allowed",
		filepath.Join(dir, "gone.png"): "no such file",
		empty:                          "file is empty",
		big:                            "larger than 10.0 MB",
	} {
		if _, err = Upload(ctx, http.DefaultClient, f.srv.URL, "tok", path); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", path, err, want)
		}
	}
	if f.last != nil {
		t.Error("a locally refused upload reached the server")
	}
	// A directory with an allowed extension fails at Stat/Read, not silently.
	_ = os.MkdirAll(filepath.Join(dir, "folder.pdf"), 0o700)
	if _, err = Upload(ctx, http.DefaultClient, f.srv.URL, "tok", filepath.Join(dir, "folder.pdf")); err == nil {
		t.Error("directory uploaded")
	}
	if _, err = Upload(ctx, http.DefaultClient, "::bad url", "tok", file); err == nil {
		t.Error("bad url swallowed")
	}
	if _, err = Upload(ctx, http.DefaultClient, "http://127.0.0.1:1", "tok", file); err == nil {
		t.Error("transport error swallowed")
	}
}
