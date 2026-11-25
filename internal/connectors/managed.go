package connectors

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"
)

type WriteError struct{ Outcome, Code string }

func (e *WriteError) Error() string { return e.Outcome + ": " + e.Code }
func Rejected(reason string) error  { return &WriteError{"rejected", reason} }
func Unknown(reason string) error   { return &WriteError{"unknown", reason} }
func WriteRejected(err error) bool {
	var e *WriteError
	return errors.As(err, &e) && e.Outcome == "rejected"
}

type submissionKey struct{}

// Only the application submission owner supplies this after persisting intent.
func WithSubmission(ctx context.Context) context.Context {
	return context.WithValue(ctx, submissionKey{}, true)
}
func (r *runtime) managedWrites() bool { return r.config.Kind == "email" || r.config.Kind == "sheets" }
func (m *Manager) ManagedWrites(id string) bool {
	r, err := m.get(id)
	return err == nil && r.managedWrites()
}
func (m *Manager) Projects(id string) []string {
	r, err := m.get(id)
	if err != nil {
		return nil
	}
	return slices.Clone(r.config.Projects)
}

type authorityKey struct{}

func WithAuthority(ctx context.Context, check func(context.Context) error) context.Context {
	return context.WithValue(ctx, authorityKey{}, check)
}

// checkAuthority rechecks the caller's current grant at outbound boundaries.
// Standalone operator clients without a callback retain their configured policy.
func checkAuthority(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if check, ok := ctx.Value(authorityKey{}).(func(context.Context) error); ok {
		return check(ctx)
	}
	return nil
}

func managedOperation(kind string, op *Operation) error {
	definitions := map[string]map[string]string{
		"email":  {"status": "", "messages": "filter,providerQuery,pageToken,pageSize", "message": "id", "thread": "id", "attachment": "messageId,attachmentId", "changes": "cursor,pageToken,pageSize", "selection": "id,filter,providerQuery", "draft": "id", "draft-create": "actionID,to,subject,text,replyToMessage,threadRevision", "draft-update": "actionID,id,expectedFingerprint,to,subject,text,replyToMessage,threadRevision", "send": "actionID,to,subject,text,replyToMessage,threadRevision", "draft-send": "actionID,id,expectedFingerprint,threadRevision"},
		"sheets": {"status": "", "metadata": "", "read": "ranges", "update": "actionID,range,values,identityColumn,identity,expected", "append": "actionID,range,values,identityColumn,identity,expected"},
		// A calendar connection only reads: dates are YYYY-MM-DD.
		"calendar": {"status": "", "events": "from,to,pageToken"},
	}
	args, ok := definitions[kind][op.Name]
	if !ok {
		return errors.New("unsupported managed operation")
	}
	read := !slices.Contains([]string{"draft-create", "draft-update", "send", "draft-send", "update", "append"}, op.Name)
	if op.ReadOnly != read {
		return errors.New("managed operation read/write grant does not match its behavior")
	}
	if op.Path != "" || op.Method != "" || op.BodyArgument != "" || op.Response != "" || len(op.Query) > 0 || len(op.QueryLists) > 0 || op.Body || op.Range != nil {
		return errors.New("managed operation routes are adapter-owned")
	}
	properties := map[string]any{}
	for _, name := range strings.Split(args, ",") {
		if name == "" {
			continue
		}
		var schema any = map[string]any{"type": "string"}
		if kind == "email" && name == "to" || name == "ranges" {
			schema = map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "minItems": 1}
		}
		if slices.Contains([]string{"pageSize", "identityColumn"}, name) {
			schema = map[string]any{"type": "integer", "minimum": 1}
		}
		if name == "values" || name == "expected" {
			schema = map[string]any{"type": "array", "items": map[string]any{"type": "array"}}
		}
		if name == "filter" {
			schema = emailFilterSchema()
		}
		if name == "providerQuery" {
			schema = map[string]any{"type": "object", "properties": map[string]any{"provider": map[string]any{"type": "string"}, "query": map[string]any{"type": "string"}}, "required": []string{"provider", "query"}, "additionalProperties": false}
		}
		properties[name] = schema
	}
	required := []string{}
	switch op.Name {
	case "message", "thread", "draft":
		required = []string{"id"}
	case "selection":
		required = []string{"id"}
	case "attachment":
		required = []string{"messageId", "attachmentId"}
	case "changes":
		required = []string{"cursor"}
	case "read":
		required = []string{"ranges"}
	case "events":
		required = []string{"from", "to"}
	case "draft-create", "send":
		required = []string{"actionID", "to", "subject", "text"}
	case "draft-update":
		required = []string{"actionID", "id", "expectedFingerprint", "to", "subject", "text"}
	case "draft-send":
		required = []string{"actionID", "id", "expectedFingerprint", "threadRevision"}
	case "update", "append":
		required = []string{"actionID", "range", "values", "identityColumn", "identity", "expected"}
	}
	op.InputSchema, _ = json.Marshal(map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false})
	op.Description = kind + " " + op.Name + " using the configured identity and host policy. Pagination is explicit; writes require a durable actionID."
	return nil
}

func (m *Manager) CheckAccount(ctx context.Context, id string) error {
	r, err := m.get(id)
	if err != nil {
		return err
	}
	if !r.managedWrites() {
		return errors.New("connection does not expose a checked account")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(r.config.TimeoutSeconds)*time.Second)
	defer cancel()
	token, _, err := r.refreshedToken(ctx)
	if err != nil {
		return err
	}
	_, _, err = r.googleIdentity(ctx, token)
	return err
}
