package connectors

import (
	"encoding/json"
	"strings"
)

// Reuse the managed email schemas so personal and operator-configured accounts
// expose the same checked contracts to agents.
func PersonalOperations() []map[string]any {
	arguments := map[string]string{
		"calendar.list": "pageToken", "calendar.events": "calendar,timeMin,timeMax,pageToken",
		"calendar.event": "calendar,id", "calendar.create": "actionID,event", "calendar.update": "actionID,calendar,id,etag,event",
		"documents.search": "query,pageToken", "documents.read": "id", "sheets.read": "id,ranges",
		"slack.conversations": "cursor", "slack.history": "channel,oldest,cursor", "slack.thread": "channel,ts,cursor",
		"slack.permalink": "channel,ts", "slack.send": "actionID,channel,text,thread_ts,expectedRevision",
	}
	result := []map[string]any{}
	for _, name := range PersonalOperationNames() {
		provider := "google"
		if strings.HasPrefix(name, "slack.") {
			provider = "slack"
		}
		var schema json.RawMessage
		if strings.HasPrefix(name, "email.") {
			op := Operation{Name: strings.TrimPrefix(name, "email."), ReadOnly: PersonalRead(name)}
			if managedOperation("email", &op) != nil {
				continue
			}
			schema = op.InputSchema
		} else {
			properties := map[string]any{}
			for _, arg := range strings.Split(arguments[name], ",") {
				var value any = map[string]any{"type": "string"}
				if arg == "ranges" {
					value = map[string]any{"type": "array", "items": map[string]string{"type": "string"}}
				}
				if arg == "event" {
					value = map[string]any{"type": "object", "description": "Google Calendar event fields; update requires the current event etag."}
				}
				properties[arg] = value
			}
			schema, _ = json.Marshal(map[string]any{"type": "object", "properties": properties, "additionalProperties": false})
		}
		result = append(result, map[string]any{"provider": provider, "operation": name, "readOnly": PersonalRead(name), "inputSchema": schema})
	}
	return result
}
