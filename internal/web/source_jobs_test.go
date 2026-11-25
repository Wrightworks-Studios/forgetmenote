package web

import (
	"encoding/base64"
	"encoding/json"
	"forgetmenote/internal/catalog"
	"strings"
	"testing"
	"time"
)

func TestSourceJobPreviewSaveAndNotebookIsolation(t *testing.T) {
	app, _ := fixture(t)
	input, _ := json.Marshal(map[string]any{"filename": "queued.txt", "data": base64.StdEncoding.EncodeToString([]byte("Amber durable evidence"))})
	created := call(app, "POST", "/api/sources/jobs", string(input), testToken)
	if created.Code != 200 {
		t.Fatalf("source job creation: %d %s", created.Code, created.Body.String())
	}
	var out struct {
		Job struct {
			ID    string `json:"id"`
			State string `json:"state"`
		} `json:"job"`
		Prepared *preparedSource `json:"prepared"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &out); err != nil || out.Job.ID == "" {
		t.Fatal("missing job id", err)
	}
	id := out.Job.ID
	deadline := time.Now().Add(3 * time.Second)
	for out.Prepared == nil && time.Now().Before(deadline) {
		response := call(app, "GET", "/api/sources/jobs/"+id, "", testToken)
		if response.Code != 200 {
			t.Fatal(response.Code, response.Body.String())
		}
		if err := json.Unmarshal(response.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if out.Prepared == nil || !strings.Contains(out.Prepared.Preview, "Amber durable evidence") {
		t.Fatalf("job did not produce reusable preview: %+v", out)
	}
	if err := app.catalog.Create(catalog.Bootstrap, "other", "Other"); err != nil {
		t.Fatal(err)
	}
	isolated := call(app, "GET", "/p/other/api/sources/jobs/"+id, "", testToken)
	if isolated.Code != 403 {
		t.Fatalf("cross-notebook job exposed: %d", isolated.Code)
	}
	body, _ := json.Marshal(map[string]any{"job": id, "title": "Completed source"})
	saved := call(app, "POST", "/api/sources/import", string(body), testToken)
	if saved.Code != 200 || !strings.Contains(saved.Body.String(), "Amber durable evidence") {
		t.Fatalf("completed job save: %d %s", saved.Code, saved.Body.String())
	}
}
