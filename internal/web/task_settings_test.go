package web

import (
	"encoding/json"
	"slices"
	"testing"

	"forgetmenote/internal/catalog"
)

// The Administration page reads and changes task queue settings through the
// same handlers as MCP clients, with the browser session's own checks.
func TestTaskQueueSettingsFromTheBrowser(t *testing.T) {
	app, _ := fixture(t)
	admin := recoverySession(t, app)
	addLocal(t, app, admin, "queue-reader")
	reader := passwordSession(t, app, "queue-reader")
	if w := localRequest(app, "/integrations/actions/set_task_queues", `{"project":"","revision":0}`, reader); w.Code != 403 {
		t.Fatalf("a member changed task queues: %d %s", w.Code, w.Body.String())
	}
	if w := localRequest(app, "/integrations/actions/set_task_queues", `{"project":"","revision":0,"awayStatuses":["OOO"]}`, admin); w.Code != 200 {
		t.Fatalf("enable: %d %s", w.Code, w.Body.String())
	}
	w := localRequest(app, "/integrations/actions/task_queues", `{}`, admin)
	var settings catalog.TaskQueues
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &settings) != nil {
		t.Fatalf("read: %d %s", w.Code, w.Body.String())
	}
	if settings.Revision != 1 || settings.Project != catalog.TaskQueuesProject || !slices.Equal(settings.AwayStatuses, []string{"ooo"}) {
		t.Fatalf("settings: %+v", settings)
	}
	if w = localRequest(app, "/integrations/actions/set_task_queues", `{"project":"","revision":0}`, admin); w.Code != 409 {
		t.Fatalf("a stale revision was saved: %d %s", w.Code, w.Body.String())
	}
}
