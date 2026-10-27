package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stefan-zemljic/data-organizer/internal/store"
)

func post(t *testing.T, h http.Handler, target, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	return send(t, h, http.MethodPost, target, body, headers)
}

func send(t *testing.T, h http.Handler, method, target, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Host = host
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(keyCookie)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func startBody(t *testing.T, roots ...string) string {
	b, err := json.Marshal(map[string]any{"roots": roots})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestBackupAPI(t *testing.T) {
	h := newHandler(t)
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "f.txt"), []byte("abc"), 0o644)

	sc := backupToEnd(t, h, root)
	if sc.Status != store.StatusCompleted || sc.Progress.FilesDone != 1 || sc.Progress.BytesDone != 3 || !sc.Ready {
		t.Fatalf("backup = %+v", sc)
	}

	// Entries: the root, then its child.
	rec := get(t, h, "/api/backups/"+itoa(sc.ID)+"/entries/roots", host)
	var roots struct{ Entries []store.BackupEntry }
	json.Unmarshal(rec.Body.Bytes(), &roots)
	if rec.Code != http.StatusOK || len(roots.Entries) != 1 || roots.Entries[0].Name != root {
		t.Fatalf("entry roots: %d %s", rec.Code, rec.Body)
	}
	children := "/api/backups/" + itoa(sc.ID) + "/entries/children?parent=" + itoa(roots.Entries[0].ID)
	rec = get(t, h, children+"&offset=0&limit=10", host)
	var page store.EntryPage
	json.Unmarshal(rec.Body.Bytes(), &page)
	if rec.Code != http.StatusOK || page.Total != 1 || page.Entries[0].Name != "f.txt" || page.Entries[0].DupCount != 0 {
		t.Errorf("entry children: %d %s", rec.Code, rec.Body)
	}
	if rec := get(t, h, children+"&offset=0&limit=1001", host); rec.Code != http.StatusBadRequest {
		t.Errorf("limit 1001: %d", rec.Code)
	}

	// Marks: exclude the (unique) file, check it, include it again.
	entries := "/api/backups/" + itoa(sc.ID) + "/entries"
	body := `{"items":[` + itoa(page.Entries[0].ID) + `],"on":true}`
	if rec := post(t, h, entries+"/exclude", body, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("exclude: %d %s", rec.Code, rec.Body)
	}
	rec = get(t, h, children+"&offset=0&limit=10", host)
	json.Unmarshal(rec.Body.Bytes(), &page)
	if !page.Entries[0].Excluded {
		t.Error("file not excluded")
	}
	rec = post(t, h, entries+"/state", body, nil)
	var state struct{ Entries []store.BackupEntry }
	json.Unmarshal(rec.Body.Bytes(), &state)
	if rec.Code != http.StatusOK || len(state.Entries) != 1 || !state.Entries[0].ExcludedSelf || state.Entries[0].NotFirst {
		t.Errorf("state: %d %s", rec.Code, rec.Body)
	}
	rec = post(t, h, entries+"/reset", `{}`, nil)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"removed":1}` {
		t.Errorf("reset: %d %s", rec.Code, rec.Body)
	}
	if rec := post(t, h, entries+"/exclude", `{"on":true}`, nil); rec.Code != http.StatusBadRequest {
		t.Errorf("missing items: %d, want 400", rec.Code)
	}
	if rec := get(t, h, "/api/backups/"+itoa(sc.ID)+"/entries/children?parent=999999&offset=0&limit=10", host); rec.Code != http.StatusNotFound {
		t.Errorf("foreign parent: %d", rec.Code)
	}

	rec = get(t, h, "/api/backups/current", host)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"backup":null}` {
		t.Errorf("current: %d %s", rec.Code, rec.Body)
	}

	rec = get(t, h, "/api/backups", host)
	var list struct{ Backups []store.Backup }
	json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list.Backups) != 1 || len(list.Backups[0].Roots) != 1 || list.Backups[0].Roots[0] != root {
		t.Errorf("list = %+v", list)
	}

	if rec := post(t, h, "/api/backups/"+itoa(sc.ID)+"/pause", "", nil); rec.Code != http.StatusConflict {
		t.Errorf("pause finished backup: %d", rec.Code)
	}
	if rec := get(t, h, "/api/backups/999", host); rec.Code != http.StatusNotFound {
		t.Errorf("missing backup: %d", rec.Code)
	}
}

func TestBackupAPIRejectsBadInput(t *testing.T) {
	h := newHandler(t)
	if rec := post(t, h, "/api/backups", startBody(t, "relative"), nil); rec.Code != http.StatusBadRequest {
		t.Errorf("relative root: %d", rec.Code)
	}
	if rec := post(t, h, "/api/backups", startBody(t), nil); rec.Code != http.StatusBadRequest {
		t.Errorf("no roots: %d", rec.Code)
	}
	if rec := post(t, h, "/api/backups", "{", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("bad JSON: %d", rec.Code)
	}
}

func TestCrossSiteBackupStartRejected(t *testing.T) {
	h := newHandler(t)
	rec := post(t, h, "/api/backups", startBody(t, t.TempDir()), map[string]string{
		"Sec-Fetch-Site": "cross-site",
		"Origin":         "https://evil.example",
	})
	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-site POST: %d, want 403", rec.Code)
	}
	rec = get(t, h, "/api/backups", host)
	if !strings.Contains(rec.Body.String(), `"backups":[]`) {
		t.Errorf("a backup was created: %s", rec.Body)
	}
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// backupToEnd starts a backup of root and waits until it has finished.
func backupToEnd(t *testing.T, h http.Handler, root string) store.Backup {
	t.Helper()
	rec := post(t, h, "/api/backups", startBody(t, root), nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	var sc store.Backup
	json.Unmarshal(rec.Body.Bytes(), &sc)
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		rec = get(t, h, "/api/backups/"+itoa(sc.ID), host)
		json.Unmarshal(rec.Body.Bytes(), &sc)
		if sc.Status == store.StatusCompleted {
			break
		}
	}
	return sc
}

func TestDeleteBackupAPI(t *testing.T) {
	h := newHandler(t)
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "f.txt"), []byte("abc"), 0o644)
	sc := backupToEnd(t, h, root)
	target := "/api/backups/" + itoa(sc.ID)

	crossSite := map[string]string{"Origin": "https://evil.example", "Sec-Fetch-Site": "cross-site"}
	if rec := send(t, h, http.MethodDelete, target, "", crossSite); rec.Code != http.StatusForbidden {
		t.Errorf("cross-site delete: %d, want 403", rec.Code)
	}
	rec := send(t, h, http.MethodDelete, target, "", nil)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"deleted":{"items":2,"restores":0,"names":2,"contents":1,"bytesFreed":3}}` {
		t.Errorf("delete: %d %s", rec.Code, rec.Body)
	}
	if rec := get(t, h, target, host); rec.Code != http.StatusNotFound {
		t.Errorf("get after delete: %d, want 404", rec.Code)
	}
	if rec := send(t, h, http.MethodDelete, target, "", nil); rec.Code != http.StatusNotFound {
		t.Errorf("second delete: %d, want 404", rec.Code)
	}
}
