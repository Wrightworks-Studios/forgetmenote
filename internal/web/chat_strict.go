package web

import (
	"errors"
	"forgetmenote/internal/chat"
)

func normalizeStrictToolCall(tools []chat.Tool, connection chat.Connection, call chat.ToolCall) (chat.ToolCall, error) {
	call = modelDataCall(call)
	if !connection.Capabilities.StrictTools {
		return call, nil
	}
	for _, tool := range tools {
		if tool.Name == call.Name {
			arguments, err := chat.NormalizeStrictToolArguments(tool.Schema, call.Arguments)
			if err != nil {
				return call, err
			}
			call.Arguments = arguments
			return call, nil
		}
	}
	return call, errors.New("tool was not exposed for this run")
}
