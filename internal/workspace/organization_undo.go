package workspace

import (
	"encoding/json"
)

type OrganizationUndo struct {
	Executor string             `json:"executor,omitempty"`
	ID       string             `json:"id"`
	Action   OrganizationAction `json:"action"`
	Actor    string             `json:"actor"`
	Revision string             `json:"revision"`
	Later    []OrganizationUndo `json:"later,omitempty"`
}

func undoOperation(input OrganizationUndo, actor string) operationIdentity {
	if input.Executor != "" {
		actor = input.Executor
	}
	// Later receipts authorize restoration order, not a new mutation identity.
	input.Later = nil
	raw, _ := json.Marshal(input)
	return operationIdentity{ID: ContentHash([]byte(actor + "\x00organization-undo:" + input.ID)), Request: ContentHash(raw)}
}
func strconvQuote(value string) string { raw, _ := json.Marshal(value); return string(raw) }
