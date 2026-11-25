package chat_test

import (
	"context"
	"testing"

	"forgetmenote/internal/chat"
)

// Another package can build, copy and bind a run scope, but only WithApproval
// marks the person's approval, and it leaves the scope it copied unapproved.
func TestOnlyWithApprovalApproves(t *testing.T) {
	ctx := chat.WithScope(context.Background(), &chat.RunScope{Owner: "alice", Project: "default"})
	if chat.RunScopeFrom(ctx).Approved() {
		t.Fatal("a bound scope starts approved")
	}
	copied := *chat.RunScopeFrom(ctx)
	if chat.RunScopeFrom(chat.WithScope(ctx, &copied)).Approved() {
		t.Fatal("a copy of an unapproved scope is approved")
	}
	approved := chat.WithApproval(ctx)
	if !chat.RunScopeFrom(approved).Approved() {
		t.Fatal("WithApproval did not approve")
	}
	if chat.RunScopeFrom(ctx).Approved() {
		t.Fatal("WithApproval changed the scope it copied")
	}
	if chat.RunScopeFrom(chat.WithoutRunScope(approved)) != nil {
		t.Fatal("WithoutRunScope kept a scope")
	}
	if chat.RunScopeFrom(chat.WithApproval(context.Background())) != nil {
		t.Fatal("WithApproval made a scope outside a chat run")
	}
}
