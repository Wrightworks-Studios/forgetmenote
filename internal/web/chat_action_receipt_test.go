package web

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/chat"
)

func TestChatApprovedMutationReceiptSurvivesRevocationAndRestart(t *testing.T) {
	const private = "PRIVATE_ACKNOWLEDGED_CONTAINER_RESULT"
	var modelCalls atomic.Int32
	app, c := runnerFixture(t, func(w http.ResponseWriter, r *http.Request) {
		modelCalls.Add(1)
		io.Copy(io.Discard, r.Body)
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"The approved action is complete."},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":5}}`)
	})
	if err := app.catalog.Member(catalog.Bootstrap, "default", "alice", "edit"); err != nil {
		t.Fatal(err)
	}
	store, err := app.catalog.Access("alice", "default", "edit")
	if err != nil {
		t.Fatal(err)
	}
	note, err := store.Save(t.Context(), "notes/approved.md", []byte("# "+private), "", "alice")
	if err != nil {
		t.Fatal(err)
	}
	connection, err := app.chat.Config.Resolve("local", "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	options := c.Options
	options.Mode = "edit"
	options.Actions = []string{"notes"}
	options.BudgetTokens = connection.Limits.RunTokens
	auth := chat.RunAuth{User: "alice", Project: "default", Epoch: app.catalog.Epoch("alice")}
	arguments, _ := json.Marshal(map[string]string{"path": note.Path, "revision": note.Revision, "container": "archive"})
	call := chat.ToolCall{ID: "archive", Name: "set_note_container", Arguments: arguments}
	run := chat.Run{ID: chat.NewID(), State: "running", Epoch: auth.Epoch, Options: options, Rounds: 1, RoundLimit: connection.Limits.MaxRounds, ToolLimit: connection.Limits.MaxTools, Usage: chat.Accounting{Reserved: int64(options.BudgetTokens)}, Messages: []chat.Message{{Role: "user", Content: "Archive the requested note."}, {Role: "assistant", ToolCalls: []chat.ToolCall{call}}}}
	if err = app.chat.Store.Update("alice", "default", c.ID, func(saved *chat.Conversation) error {
		saved.Turns = append(saved.Turns, chat.Turn{ID: chat.NewID(), RunID: run.ID, Role: "user", Content: "Archive the requested note."})
		saved.Runs = append(saved.Runs, run)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ctx, err := app.runContext(auth)
	if err != nil {
		t.Fatal(err)
	}
	if err = app.executeNotebookTool(ctx, auth, c.ID, run.ID, call, connection); !errors.Is(err, errChatConfirmation) {
		t.Fatalf("approval not prepared: %v", err)
	}
	prepared, err := app.chat.Store.Read("alice", "default", c.ID)
	if err != nil {
		t.Fatal(err)
	}
	operation := prepared.Run(run.ID).Operations[0]
	if operation.State != "awaiting_confirmation" || operation.Confirmation == "" {
		t.Fatal("missing exact confirmation")
	}
	action := app.actions[call.Name]
	actual := action.Call
	var mutations atomic.Int32
	acknowledged := make(chan json.RawMessage, 1)
	action.Call = func(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
		mutations.Add(1)
		result, err := actual(ctx, args)
		if err != nil {
			return result, err
		}
		acknowledged <- result
		if err = app.catalog.Member(catalog.Bootstrap, "default", "alice", ""); err != nil {
			return nil, err
		}
		return result, nil
	}
	app.actions[call.Name] = action
	server := httptest.NewServer(app)
	defer server.Close()
	app.origin = server.URL
	client := &http.Client{Timeout: 5 * time.Second}
	request := func(method, path, body string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(method, server.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Remote-User", "alice")
		req.Header.Set("X-ForgetMeNote-Proxy-Secret", proxyTestSecret)
		req.Header.Set("Origin", server.URL)
		req.Header.Set("Content-Type", "application/json")
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	read := func(response *http.Response) (int, []byte) {
		t.Helper()
		defer response.Body.Close()
		raw, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, raw
	}
	path := "/p/default/api/chat/" + c.ID
	stream := request("GET", path+"/events", "")
	defer stream.Body.Close()
	if stream.StatusCode != 200 {
		t.Fatalf("SSE setup status=%d", stream.StatusCode)
	}
	reader := bufio.NewReader(stream.Body)
	// Receiving the first frame proves the authorized stream is open before revocation.
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\n" {
			break
		}
	}
	type streamResult struct {
		raw []byte
		err error
	}
	ended := make(chan streamResult, 1)
	go func() { raw, err := io.ReadAll(reader); ended <- streamResult{raw, err} }()
	approval, _ := json.Marshal(map[string]string{"decision": "approve", "confirmation": operation.Confirmation})
	code, body := read(request("POST", path+"/actions/"+operation.ID, string(approval)))
	app.chat.Wait()
	if code != 403 || bytes.Contains(body, []byte(private)) {
		t.Fatalf("revoked approval disclosed outcome: %d %s", code, body)
	}
	var result json.RawMessage
	select {
	case result = <-acknowledged:
	default:
		t.Fatal("real mutation was not acknowledged")
	}
	if !bytes.Contains(result, []byte(private)) {
		t.Fatal("fixture acknowledgement omitted private result")
	}
	select {
	case received := <-ended:
		if received.err != nil || bytes.Contains(received.raw, []byte(private)) || bytes.Contains(received.raw, []byte("tool_result")) {
			t.Fatalf("revoked stream received result: %s / %v", received.raw, received.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("revoked stream did not close")
	}
	for _, suffix := range []string{"", "/events"} {
		code, body = read(request("GET", path+suffix, ""))
		if code != 403 || bytes.Contains(body, []byte(private)) {
			t.Fatalf("revoked read disclosure: %d %s", code, body)
		}
	}
	saved, err := app.chat.Store.Read("alice", "default", c.ID)
	if err != nil {
		t.Fatal(err)
	}
	receipt := saved.Run(run.ID).Operations[0]
	if receipt.State != "completed" || !receipt.Approved || receipt.Confirmation != "" || receipt.Error != "" || !bytes.Equal(receipt.Result, result) {
		t.Fatalf("acknowledged private receipt lost: %+v", receipt)
	}
	if saved.Run(run.ID).State != "interrupted" || saved.Run(run.ID).Usage.Reserved != 0 {
		t.Fatal("approved control did not pause and release reservation")
	}
	for _, message := range saved.Run(run.ID).Messages {
		if message.Role == "tool" {
			t.Fatal("revoked result entered model continuation")
		}
	}
	if modelCalls.Load() != 0 {
		t.Fatal("approved action automatically continued model work")
	}
	committed, err := store.Read(t.Context(), note.Path, "")
	if err != nil || committed.Revision == note.Revision || !bytes.Contains(committed.Content, []byte("fmn-container: archive")) {
		t.Fatalf("acknowledged Git effect missing: %v", err)
	}
	// Restart durable chat ownership, then recover through the public read/resume paths.
	app.chat.Store.Close()
	reopened, err := chat.OpenStore(app.db)
	if err != nil {
		t.Fatal(err)
	}
	app.chat.Store = reopened
	recovered, err := reopened.Read("alice", "default", c.ID)
	if err != nil || recovered.Run(run.ID).Operations[0].State != "completed" || !bytes.Equal(recovered.Run(run.ID).Operations[0].Result, result) {
		t.Fatalf("receipt lost after restart: %v", err)
	}
	if err = app.catalog.Member(catalog.Bootstrap, "default", "alice", "edit"); err != nil {
		t.Fatal(err)
	}
	code, body = read(request("GET", path, ""))
	if code != 200 {
		t.Fatalf("restored access cannot read recovered run: %d %s", code, body)
	}
	code, body = read(request("POST", path+"/actions/"+operation.ID, string(approval)))
	if code != 409 {
		t.Fatalf("consumed approval became reusable: %d %s", code, body)
	}
	before, err := store.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	code, body = read(request("POST", path+"/runs/"+run.ID+"/resume", `{}`))
	if code != 200 {
		t.Fatalf("explicit continuation failed: %d %s", code, body)
	}
	done := waitChatRun(t, app, c.ID)
	app.chat.Wait()
	after, err := store.Status(t.Context())
	if err != nil || before.Revision != after.Revision || mutations.Load() != 1 || done.Run(run.ID).State != "completed" || modelCalls.Load() != 1 {
		t.Fatalf("recovery repeated mutation or failed continuation: mutations=%d models=%d state=%s err=%v", mutations.Load(), modelCalls.Load(), done.Run(run.ID).State, err)
	}
}
