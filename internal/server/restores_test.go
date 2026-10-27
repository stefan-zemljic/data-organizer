package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stefan-zemljic/data-organizer/internal/store"
)

func TestRestoreAPI(t *testing.T) {
	h := newHandler(t)
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "f.txt"), []byte("abc"), 0o644)

	rec := post(t, h, "/api/backups", startBody(t, root), nil)
	var sc store.Backup
	json.Unmarshal(rec.Body.Bytes(), &sc)
	for deadline := time.Now().Add(10 * time.Second); !sc.Ready && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		json.Unmarshal(get(t, h, "/api/backups/"+itoa(sc.ID), host).Body.Bytes(), &sc)
	}

	target := filepath.Join(t.TempDir(), "out")
	body, _ := json.Marshal(map[string]string{"target": target})
	rec = post(t, h, "/api/backups/"+itoa(sc.ID)+"/restore", string(body), nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("start restore: %d %s", rec.Code, rec.Body)
	}
	var cp store.Restore
	json.Unmarshal(rec.Body.Bytes(), &cp)
	for deadline := time.Now().Add(10 * time.Second); cp.Status == store.RestoreRunning && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		json.Unmarshal(get(t, h, "/api/restores/"+itoa(cp.ID), host).Body.Bytes(), &cp)
	}
	if cp.Status != store.RestoreCompleted || cp.Progress.FilesOK != 1 || cp.Progress.BytesDone != 3 {
		t.Fatalf("restore = %+v", cp)
	}

	rec = get(t, h, "/api/restores", host)
	if !strings.Contains(rec.Body.String(), `"target":`) {
		t.Errorf("list: %s", rec.Body)
	}
	if rec := get(t, h, "/api/restores/current", host); strings.TrimSpace(rec.Body.String()) != `{"restore":null}` {
		t.Errorf("current: %s", rec.Body)
	}
	rec = get(t, h, "/api/restores/"+itoa(cp.ID)+"/entries/roots", host)
	var roots struct{ Entries []store.RestoreEntry }
	json.Unmarshal(rec.Body.Bytes(), &roots)
	if len(roots.Entries) != 1 || roots.Entries[0].Status != "ok" {
		t.Fatalf("entry roots: %s", rec.Body)
	}
	rec = get(t, h, "/api/restores/"+itoa(cp.ID)+"/entries/children?parent="+itoa(roots.Entries[0].ID)+"&offset=0&limit=10", host)
	if !strings.Contains(rec.Body.String(), `"name":"f.txt"`) || !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Errorf("entry children: %s", rec.Body)
	}

	// The target is not empty any more; relative targets are invalid.
	if rec := post(t, h, "/api/backups/"+itoa(sc.ID)+"/restore", string(body), nil); rec.Code != http.StatusBadRequest {
		t.Errorf("non-empty target: %d", rec.Code)
	}
	if rec := post(t, h, "/api/backups/"+itoa(sc.ID)+"/restore", `{"target":"rel"}`, nil); rec.Code != http.StatusBadRequest {
		t.Errorf("relative target: %d", rec.Code)
	}
	for _, action := range []string{"pause", "resume", "stop"} {
		if rec := post(t, h, "/api/restores/"+itoa(cp.ID)+"/"+action, "", nil); rec.Code != http.StatusConflict {
			t.Errorf("%s finished restore: %d", action, rec.Code)
		}
	}
}
