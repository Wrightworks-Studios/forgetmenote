package web

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"forgetmenote/internal/capacity"
	"forgetmenote/internal/workspace"
	"github.com/reearth/ygo/crdt"
)

// A live update past the note limit, or one that is not Yjs, is a
// deterministic rejection that leaves the accepted live state, the Markdown
// and the notebook's revision as they were.
func TestLiveRejectionPublicBoundaryPreservesState(t *testing.T) {
	ctx := context.Background()
	app, store := fixture(t)
	original, err := store.Save(ctx, "utf8.md", []byte("accepted peer text\n"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	base, err := store.LiveRead(ctx, original.Path)
	if err != nil {
		t.Fatal(err)
	}
	doc := crdt.New(crdt.WithGC(false))
	if err = crdt.ApplyUpdateV1(doc, base.Update, nil); err != nil {
		t.Fatal(err)
	}
	// Live editing takes a note up to the ordinary file limit, lowered here
	// so the overflowing update stays small.
	t.Setenv(capacity.FileBytes.Env(), strconv.Itoa(256<<10))
	doc.Transact(func(tx *crdt.Transaction) {
		tx.GetText("markdown").Insert(tx, 0, strings.Repeat("é", workspace.MaxReadBytes(original.Path)/2), nil)
	})
	for name, update := range map[string][]byte{"utf8-overflow": crdt.EncodeStateAsUpdateV1(doc, nil), "invalid-wire": {255}} {
		in := workspace.LiveUpdate{Path: original.Path, Generation: strings.Split(base.LiveRevision, ":")[0], OperationID: name, Update: update}
		raw, err := json.Marshal(in)
		if err != nil {
			t.Fatal(err)
		}
		response := call(app, "POST", "/api/live", string(raw), testToken)
		if response.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s is not a deterministic rejection: %d %s", name, response.Code, response.Body.String())
		}
		var failure struct{ Code, Error string }
		if err = json.Unmarshal(response.Body.Bytes(), &failure); err != nil || failure.Code != "live_operation_rejected" || failure.Error == "" {
			t.Errorf("%s lacks machine-readable rejection: %s (%v)", name, response.Body.String(), err)
		}
	}
	after, err := store.LiveRead(ctx, original.Path)
	if err != nil || !reflect.DeepEqual(base, after) {
		t.Fatalf("rejected update changed accepted live state: %v", err)
	}
	committed, err := store.Read(ctx, original.Path, "")
	if err != nil || string(committed.Content) != string(original.Content) || committed.Revision != original.Revision {
		t.Fatalf("rejected update changed the Markdown or its revision: %v", err)
	}
}
