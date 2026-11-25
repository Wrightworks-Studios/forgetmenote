package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"forgetmenote/internal/catalog"
	"forgetmenote/internal/workspace"

	"github.com/reearth/ygo/crdt"
)

func TestLiveNotePublicBoundary(t *testing.T) {
	app, _ := fixture(t)
	created := call(app, "POST", "/api/note", `{"path":"live.md","content":"Hello world\n","revision":""}`, testToken)
	if created.Code != 200 {
		t.Fatal(created.Body.String())
	}
	w := call(app, "GET", "/api/live?path=live.md", "", testToken)
	if w.Code != 200 {
		t.Fatalf("live note read: %d %s", w.Code, w.Body.String())
	}
	var v struct {
		Content, LiveRevision string
		Update                []byte
	}
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v.Content != "Hello world\n" || v.LiveRevision == "" || len(v.Update) == 0 {
		t.Fatalf("live snapshot missing content, version, or Yjs state: %+v", v)
	}
	if w := call(app, "GET", "/api/live?path=live.md", "", ""); w.Code != 401 {
		t.Fatalf("unauthenticated live read: %d", w.Code)
	}
}

func TestLiveLongPollStopsWhenNotebookAccessIsRevoked(t *testing.T) {
	app, store := fixture(t)
	reader := addLocal(t, app, recoverySession(t, app), "live-reader")
	if err := app.catalog.Member(catalog.Bootstrap, "default", reader, "read"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(t.Context(), "private.md", []byte("private content\n"), "", ""); err != nil {
		t.Fatal(err)
	}
	login := oidcRequest(app, "POST", "/auth/password/login", url.Values{"username": {"live-reader"}, "password": {"test-password-long"}}.Encode())
	if login.Code != 303 {
		t.Fatal(login.Code, login.Body.String())
	}
	cookie := login.Result().Cookies()[0]
	initial := oidcRequest(app, "GET", "/p/default/api/live?path=private.md", "", cookie)
	var base workspace.LiveDocument
	if initial.Code != 200 || json.Unmarshal(initial.Body.Bytes(), &base) != nil {
		t.Fatal(initial.Code, initial.Body.String())
	}
	since := fmt.Sprintf("%s/%s/%t", base.LiveRevision, base.Version, base.Pending)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- oidcRequest(app, "GET", "/p/default/api/live?path=private.md&wait=5000&since="+url.QueryEscape(since), "", cookie)
	}()
	select {
	case response := <-done:
		t.Fatal("long poll did not wait", response.Code)
	case <-time.After(100 * time.Millisecond):
	}
	if err := app.catalog.Member(catalog.Bootstrap, "default", reader, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LivePatch(t.Context(), workspace.TargetedPatch{Path: "private.md", Base: base.LiveRevision, OperationID: "after-revocation", Edits: []workspace.TextEdit{{Start: 0, End: 0, Old: "", Text: "new secret after revocation\n"}}}); err != nil {
		t.Fatal(err)
	}
	select {
	case response := <-done:
		if response.Code != 403 || strings.Contains(response.Body.String(), "private content") || strings.Contains(response.Body.String(), "new secret") {
			t.Fatalf("revoked response %d %s", response.Code, response.Body.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("revocation did not cancel long poll")
	}
}

func TestLiveLongPollWaitsForDurablePatch(t *testing.T) {
	app, store := fixture(t)
	if _, err := store.Save(t.Context(), "live.md", []byte("alpha\n"), "", ""); err != nil {
		t.Fatal(err)
	}
	initial := call(app, "GET", "/api/live?path=live.md", "", testToken)
	var base workspace.LiveDocument
	if err := json.Unmarshal(initial.Body.Bytes(), &base); err != nil {
		t.Fatal(err)
	}
	since := fmt.Sprintf("%s/%s/%t", base.LiveRevision, base.Version, base.Pending)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- call(app, "GET", "/api/live?path=live.md&wait=1000&since="+url.QueryEscape(since), "", testToken)
	}()
	select {
	case response := <-done:
		t.Fatalf("unchanged long poll returned before an update: %s", response.Body.String())
	case <-time.After(100 * time.Millisecond):
	}
	accepted, err := store.LivePatch(t.Context(), workspace.TargetedPatch{Path: "live.md", Base: base.LiveRevision, OperationID: "wake-reader", Edits: []workspace.TextEdit{{Start: 0, End: 5, Old: "alpha", Text: "beta"}}})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case response := <-done:
		var got workspace.LiveDocument
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &got) != nil || got.LiveRevision != accepted.LiveRevision || got.Content != "beta\n" || !got.Pending {
			t.Fatalf("durable live update not delivered: %d %s", response.Code, response.Body.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("long poll did not wake for the uncommitted patch")
	}
}

// callAnswering sends a browser's live update or checkpoint to path, naming
// in its headers the live revision the browser holds and its tab.
func callAnswering(app *Server, path, body, since, tab string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "http://localhost/p/default"+path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+testToken)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(liveSinceHeader, since)
	r.Header.Set(liveTabHeader, tab)
	w := httptest.NewRecorder()
	serveMCPTest(app, w, r)
	return w
}

// A tab names itself in its long poll and its updates, and its long poll is
// not answered with its own update, which the update's answer brought.
func TestLiveLongPollOfATabIsNotAnsweredWithItsOwnUpdate(t *testing.T) {
	app, store := fixture(t)
	if _, err := store.Save(t.Context(), "live.md", []byte("alpha\n"), "", ""); err != nil {
		t.Fatal(err)
	}
	var base workspace.LiveDocument
	if err := json.Unmarshal(call(app, "GET", "/api/live?path=live.md", "", testToken).Body.Bytes(), &base); err != nil {
		t.Fatal(err)
	}
	since := url.QueryEscape(fmt.Sprintf("%s/%s/%t", base.LiveRevision, base.Version, base.Pending))
	polls := map[string]chan *httptest.ResponseRecorder{}
	for _, tab := range []string{"writer", "reader"} {
		poll := make(chan *httptest.ResponseRecorder, 1)
		polls[tab] = poll
		go func() {
			poll <- call(app, "GET", "/api/live?path=live.md&wait=1500&incremental=true&tab="+tab+"&since="+since, "", testToken)
		}()
	}
	// A poll that starts after the update's answer was sent waits as well.
	time.Sleep(100 * time.Millisecond)
	browser := crdt.New(crdt.WithGC(false))
	if err := crdt.ApplyUpdateV1(browser, base.Update, nil); err != nil {
		t.Fatal(err)
	}
	known := browser.StateVector()
	browser.Transact(func(tx *crdt.Transaction) { tx.GetText("markdown").Insert(tx, 5, "!", nil) })
	raw, _ := json.Marshal(workspace.LiveUpdate{Path: "live.md", Generation: strings.Split(base.LiveRevision, ":")[0], OperationID: "typed", Update: crdt.EncodeStateAsUpdateV1(browser, known)})
	if ack := callAnswering(app, "/api/live", string(raw), base.LiveRevision, "writer"); ack.Code != 200 {
		t.Fatalf("update: %d %s", ack.Code, ack.Body.String())
	}
	var reader, writer workspace.LiveDocument
	if err := json.Unmarshal((<-polls["reader"]).Body.Bytes(), &reader); err != nil || reader.Unchanged || !reader.Incremental {
		t.Fatalf("the reader's poll: %+v %v", reader, err)
	}
	if err := json.Unmarshal((<-polls["writer"]).Body.Bytes(), &writer); err != nil || !writer.Unchanged {
		t.Fatalf("the writer's poll was answered with its own update: %+v %v", writer, err)
	}
}

// A browser that asks for incremental answers gets, to its long poll, to its
// own update and to its checkpoint, only the Yjs update since the live
// revision it names; the request forms of the previous JavaScript, which a
// tab keeps across a deploy, still get the whole note. An update and a
// checkpoint name the revision in a header, which a gateway of an earlier
// release ignores, and not in their bodies, which it decodes strictly.
func TestLiveAnswersAreIncrementalOnlyWhenAsked(t *testing.T) {
	app, store := fixture(t)
	large := "# Manuscript\n\n" + strings.Repeat("Call me Ishmael. Some years ago, never mind how long precisely.\n\n", 2000)
	if _, err := store.Save(t.Context(), "book.md", []byte(large), "", ""); err != nil {
		t.Fatal(err)
	}
	read := func(w *httptest.ResponseRecorder) (v workspace.LiveDocument) {
		t.Helper()
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &v) != nil {
			t.Fatalf("%d %s", w.Code, w.Body.String()[:min(200, w.Body.Len())])
		}
		return v
	}
	base := read(call(app, "GET", "/api/live?path=book.md", "", testToken))
	if base.Incremental || base.Content != large {
		t.Fatalf("first read: %+v", base.Incremental)
	}
	since := url.QueryEscape(fmt.Sprintf("%s/%s/%t", base.LiveRevision, base.Version, base.Pending))
	patched, err := store.LivePatch(t.Context(), workspace.TargetedPatch{Path: "book.md", Base: base.LiveRevision, OperationID: "agent", Edits: []workspace.TextEdit{{Start: 2, End: 12, Old: "Manuscript", Text: "Moby-Dick"}}})
	if err != nil {
		t.Fatal(err)
	}
	poll := call(app, "GET", "/api/live?path=book.md&wait=1000&incremental=true&since="+since, "", testToken)
	delta := read(poll)
	if !delta.Incremental || delta.Content != "" || delta.LiveRevision != patched.LiveRevision || !delta.Pending || poll.Body.Len() > 1024 {
		t.Fatalf("incremental poll of %d bytes: %s", poll.Body.Len(), poll.Body.String()[:min(300, poll.Body.Len())])
	}
	previous := call(app, "GET", "/api/live?path=book.md&wait=1000&since="+since, "", testToken)
	if old := read(previous); old.Incremental || old.Content != patched.Content || old.LiveRevision != patched.LiveRevision || strings.Contains(previous.Body.String(), "incremental") {
		t.Fatalf("the previous long poll's answer changed: %+v", old.Incremental)
	}

	raw, _ := json.Marshal(workspace.LiveUpdate{Path: "book.md", Generation: strings.Split(base.LiveRevision, ":")[0], OperationID: "browser-edit", Update: []byte{0, 0}})
	body := string(raw)
	ack := callAnswering(app, "/api/live", body, delta.LiveRevision, "t1")
	acknowledged := read(ack)
	if !acknowledged.Incremental || acknowledged.Content != "" || ack.Body.Len() > 1024 {
		t.Fatalf("acknowledgement of %d bytes: %s", ack.Body.Len(), ack.Body.String()[:min(300, ack.Body.Len())])
	}
	// The same operation from the previous JavaScript, which names no revision.
	if v := read(call(app, "POST", "/api/live", body, testToken)); v.Incremental || v.Content != patched.Content {
		t.Fatalf("the previous update's answer changed: %+v", v.Incremental)
	}
	if refused := call(app, "POST", "/api/live", strings.TrimSuffix(body, "}")+`,"since":"`+delta.LiveRevision+`"}`, testToken); refused.Code != 400 {
		t.Fatalf("an update naming its revision in the body: %d %s", refused.Code, refused.Body.String())
	}

	checkpoint := callAnswering(app, "/api/live/checkpoint", `{"path":"book.md"}`, acknowledged.LiveRevision, "t1")
	if v := read(checkpoint); !v.Incremental || v.Content != "" || v.Pending || checkpoint.Body.Len() > 1024 {
		t.Fatalf("checkpoint answer of %d bytes: %s", checkpoint.Body.Len(), checkpoint.Body.String()[:min(300, checkpoint.Body.Len())])
	}
	// MCP clients and the previous JavaScript name no revision.
	if v := read(call(app, "POST", "/api/live/checkpoint", `{"path":"book.md"}`, testToken)); v.Incremental || v.Content != patched.Content || v.Pending {
		t.Fatalf("the previous checkpoint's answer changed: %+v", v.Incremental)
	}
}

// A long poll past the limit of the gateway's store of a notebook is a
// capacity refusal, 429 in HTTP, which the browser retries. No MCP tool
// long-polls, so none meets it.
func TestLiveSubscriptionLimitIsACapacityRefusal(t *testing.T) {
	app, store := fixture(t)
	if _, err := store.Save(t.Context(), "live.md", []byte("alpha\n"), "", ""); err != nil {
		t.Fatal(err)
	}
	cookie := recoverySession(t, app)
	base := oidcRequest(app, "GET", "/p/default/api/live?path=live.md", "", cookie)
	var v workspace.LiveDocument
	if err := json.Unmarshal(base.Body.Bytes(), &v); err != nil {
		t.Fatal(base.Code, base.Body.String())
	}
	known := fmt.Sprintf("%s/%s/%t", v.LiveRevision, v.Version, v.Pending)
	// The notebook's long polls, held by the store the app serves.
	served, err := app.catalog.Access(catalog.Bootstrap, "default", "read")
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(t.Context())
	waiting := make(chan error, 128)
	for i := 0; i < 128; i++ {
		go func() {
			// A poll the probe below beat to the last place tries again.
			_, err := served.WaitLive(ctx, "live.md", known, 25000, true)
			for errors.Is(err, workspace.ErrLiveCapacity) {
				time.Sleep(time.Millisecond)
				_, err = served.WaitLive(ctx, "live.md", known, 25000, true)
			}
			waiting <- err
		}()
	}
	deadline := time.Now().Add(10 * time.Second)
	var refused *httptest.ResponseRecorder
	for time.Now().Before(deadline) {
		// A 1 ms poll waits as a long poll does, so it is refused once the
		// others all wait.
		if refused = oidcRequest(app, "GET", "/p/default/api/live?path=live.md&wait=1&incremental=true&since="+url.QueryEscape(known), "", cookie); refused.Code != 200 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	stop()
	for i := 0; i < 128; i++ {
		<-waiting
	}
	if refused.Code != 429 || !strings.Contains(refused.Body.String(), `"code":"capacity"`) {
		t.Fatalf("a long poll past the limit: %d %s", refused.Code, refused.Body.String())
	}
}
