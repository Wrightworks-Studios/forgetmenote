package webcore

import (
	"context"
	"forgetmenote/internal/workspace"
)

// These keys bind the notebook selected and authorized by instance middleware.
// Feature handlers cannot select an arbitrary repository through a tool argument.
type NotebookStoreKey struct{}
type NotebookIDKey struct{}

func NotebookStore(ctx context.Context) *workspace.Store {
	store, _ := ctx.Value(NotebookStoreKey{}).(*workspace.Store)
	return store
}
func NotebookID(ctx context.Context) string {
	id, _ := ctx.Value(NotebookIDKey{}).(string)
	return id
}
