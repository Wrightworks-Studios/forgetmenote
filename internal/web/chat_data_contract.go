package web

import (
	"encoding/json"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/connectors"
)

// Models choose a source, operation and input values. Contract identities come
// from server-returned discovery, not from copying opaque strings between tools.
func modelDataCall(call chat.ToolCall) chat.ToolCall {
	if call.Name != "call_data_operation" {
		return call
	}
	var input map[string]json.RawMessage
	if json.Unmarshal(call.Arguments, &input) != nil {
		return call
	}
	delete(input, "schemaHash")
	call.Arguments, _ = json.Marshal(input)
	return call
}

func bindChatDataContract(call chat.ToolCall, messages []chat.Message) chat.ToolCall {
	if call.Name != "call_data_operation" {
		return call
	}
	var input map[string]json.RawMessage
	var target struct{ Connection, Operation string }
	if json.Unmarshal(call.Arguments, &input) != nil || json.Unmarshal(call.Arguments, &target) != nil || target.Connection == "" || target.Operation == "" {
		return call
	}
	discoveries := map[string]string{}
	for _, message := range messages {
		if message.Role != "assistant" {
			continue
		}
		for _, c := range message.ToolCalls {
			if c.Name != "list_data_operations" {
				continue
			}
			var in struct{ Connection string }
			if json.Unmarshal(c.Arguments, &in) == nil {
				discoveries[c.ID] = in.Connection
			}
		}
	}
	for i := len(messages) - 1; i >= 0; i-- {
		message := messages[i]
		connection, known := discoveries[message.ToolCallID]
		if message.Role != "tool" || !known || connection != target.Connection {
			continue
		}
		var operations []connectors.Operation
		if json.Unmarshal([]byte(message.Content), &operations) != nil {
			continue
		}
		for _, operation := range operations {
			if operation.Name == target.Operation && operation.SchemaHash != "" {
				input["schemaHash"], _ = json.Marshal(operation.SchemaHash)
				call.Arguments, _ = json.Marshal(input)
				return call
			}
		}
		// A newer successful catalog supersedes older discoveries, including
		// operations that were removed. Authorization is still checked at dispatch.
		return call
	}
	return call
}
