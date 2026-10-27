package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/stefan-zemljic/data-organizer/internal/backupjob"
	"github.com/stefan-zemljic/data-organizer/internal/fsbrowse"
	"github.com/stefan-zemljic/data-organizer/internal/restorejob"
	"github.com/stefan-zemljic/data-organizer/internal/scan"
	"github.com/stefan-zemljic/data-organizer/internal/store"
)

const (
	host      = "127.0.0.1:8080"
	accessKey = "test-key"
)

func newHandler(t *testing.T) http.Handler {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	static := fstest.MapFS{"index.html": {Data: []byte("<!doctype html>ui")}}
	backups := backupjob.New(st, scan.Options{})
	restores := restorejob.New(st, func() bool { _, ok := backups.Current(); return ok })
	return New(static, []string{host}, NewAccess(accessKey, "8080"), backups, restores)
}

func get(t *testing.T, h http.Handler, target, hostHeader string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Host = hostHeader
	req.AddCookie(keyCookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func listURL(path string, offset, limit string) string {
	q := url.Values{"path": {path}, "offset": {offset}, "limit": {limit}}
	return "/api/list?" + q.Encode()
}

func TestList(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := newHandler(t)

	rec := get(t, h, listURL(dir, "0", "10"), host)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q", cc)
	}
	var page fsbrowse.Page
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || page.Entries[0].Name != "f.txt" || *page.Entries[0].Size != 3 {
		t.Errorf("unexpected page %+v", page)
	}
}

func TestListErrors(t *testing.T) {
	dir := t.TempDir()
	h := newHandler(t)
	cases := []struct {
		name   string
		target string
		want   int
	}{
		{"limit too large", listURL(dir, "0", "1001"), http.StatusBadRequest},
		{"limit missing", "/api/list?path=" + url.QueryEscape(dir), http.StatusBadRequest},
		{"limit not a number", listURL(dir, "0", "ten"), http.StatusBadRequest},
		{"relative path", listURL("rel", "0", "10"), http.StatusBadRequest},
		{"missing dir", listURL(filepath.Join(dir, "nope"), "0", "10"), http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := get(t, h, tc.target, host)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d: %s", rec.Code, tc.want, rec.Body)
			}
			var body struct{ Message string }
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Message == "" {
				t.Errorf("error body without message: %s", rec.Body)
			}
		})
	}
}

func TestRootsAndStatic(t *testing.T) {
	h := newHandler(t)
	if rec := get(t, h, "/api/roots", host); rec.Code != http.StatusOK {
		t.Errorf("roots status = %d", rec.Code)
	}
	rec := get(t, h, "/", host)
	if rec.Code != http.StatusOK || rec.Body.String() != "<!doctype html>ui" {
		t.Errorf("index: %d %q", rec.Code, rec.Body)
	}
	if csp := rec.Header().Get("Content-Security-Policy"); csp == "" {
		t.Error("missing Content-Security-Policy header")
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("static Cache-Control = %q, want no-cache", cc)
	}
}

func TestForeignHostRejected(t *testing.T) {
	h := newHandler(t)
	for _, target := range []string{"/", "/api/roots"} {
		if rec := get(t, h, target, "evil.example:8080"); rec.Code == http.StatusOK {
			t.Errorf("%s served to foreign host", target)
		}
	}
}
