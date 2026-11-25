package web

import (
	"context"
	"testing"

	"forgetmenote/internal/catalog"
)

func TestCommentModerationFollowsRoleAndClientScope(t *testing.T) {
	app, _ := fixture(t)
	for user, role := range map[string]string{"commenter": "comment", "editor": "edit", "manager": "owner"} {
		if err := app.catalog.Member(catalog.Bootstrap, "default", user, role); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct {
		user            string
		resolve, delete bool
	}{{"commenter", false, false}, {"editor", true, false}, {"manager", true, true}} {
		ctx := context.WithValue(context.Background(), identityKey{}, test.user)
		if got := app.commentModerator(ctx, "default", false); got != test.resolve {
			t.Errorf("%s resolves others' comments = %v", test.user, got)
		}
		if got := app.commentModerator(ctx, "default", true); got != test.delete {
			t.Errorf("%s deletes others' comments = %v", test.user, got)
		}
		readOnly := context.WithValue(ctx, scopeKey{}, "notes:read")
		if app.commentModerator(readOnly, "default", false) || app.commentModerator(readOnly, "default", true) {
			t.Errorf("%s moderates without notes:write consent", test.user)
		}
	}
}
