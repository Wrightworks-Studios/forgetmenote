package web

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestBrowserBackupSharesTheExportBoundAndCap(t *testing.T) {
	app, _ := fixture(t)
	// Downloads already preparing their bundles hold every slot.
	for range liveExports {
		app.backupDownloads <- struct{}{}
	}
	w := call(app, "GET", "/api/backup", "", testToken)
	var detail struct{ Code string }
	if w.Code != 429 || json.Unmarshal(w.Body.Bytes(), &detail) != nil || detail.Code != "capacity" || w.Header().Get("Retry-After") == "" {
		t.Fatalf("busy backup: %d %s", w.Code, w.Body.String())
	}
	<-app.backupDownloads
	if w = call(app, "GET", "/api/backup", "", testToken); w.Code != 200 || !strings.HasPrefix(w.Body.String(), "# v2 git bundle") {
		t.Fatalf("admitted backup: %d %.50s", w.Code, w.Body.String())
	}

	app.backupBytes = 64
	w = call(app, "GET", "/api/backup", "", testToken)
	if w.Code != 413 || json.Unmarshal(w.Body.Bytes(), &detail) != nil || detail.Code != "too_large" {
		t.Fatalf("oversized backup: %d %s", w.Code, w.Body.String())
	}
	if left, _ := filepath.Glob(filepath.Join(app.data, "cache", "backups", "*")); len(left) != 0 {
		t.Fatal("temporary bundles left:", left)
	}
	// Each finished download returned its slot.
	if len(app.backupDownloads) != liveExports-1 {
		t.Fatalf("%d slots held", len(app.backupDownloads))
	}
}
