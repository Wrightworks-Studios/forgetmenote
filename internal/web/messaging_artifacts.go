package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"forgetmenote/internal/automation"
	"forgetmenote/internal/capacity"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/connectors"
	"forgetmenote/internal/messaging"
	"forgetmenote/internal/workspace"
	"io"
)

func resultArtifactTool() chat.Tool {
	return chat.Tool{Name: "create_result_artifact", Description: "Attach a CSV, XLSX workbook, JSON export or bar/line SVG chart using an existing complete data-operation receipt. Never supply values. Tables may be an array of objects, rows of objects, or columns with array rows. A successful code operation may print one such JSON table to stdout; keep separate exports in separate results. For charts select exact x/y column names; for other formats leave x/y empty. Files are sent with the final answer. Receipt IDs are also available from retained previous results. Charts reflect returned data, not semantic certification.", Schema: json.RawMessage(`{"type":"object","properties":{"receipt":{"type":"string"},"format":{"type":"string","enum":["csv","xlsx","json","bar","line"]},"x":{"type":"string"},"y":{"type":"string"},"title":{"type":"string"}},"required":["receipt","format"],"additionalProperties":false}`)}
}
func (s *Server) messageArtifact(ctx context.Context, r *automation.Run, raw json.RawMessage) (json.RawMessage, error) {
	if r.Spec.Delivery == nil || !r.Spec.Delivery.Direct && !r.Spec.Delivery.Shared {
		return nil, errors.New("file replies require a DM or approved shared channel")
	}
	var in messaging.ArtifactRequest
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&in) != nil || d.Decode(new(any)) != io.EOF {
		return nil, errors.New("invalid artifact arguments")
	}
	for _, result := range r.ChatResults {
		if result.Receipt.ID != in.Receipt {
			continue
		}
		if !s.dataAllowed(ctx, result.Receipt.Connection, result.Receipt.Operation) {
			return nil, workspace.ErrForbidden
		}
		if !result.Receipt.Complete {
			return nil, errors.New("result is incomplete; fetch a complete page or aggregate in the source first")
		}
		a, err := messaging.RenderArtifact(result.Data, in)
		if err != nil {
			return nil, err
		}
		size := len(a.Data)
		for _, old := range r.Artifacts {
			size += len(old.Data)
		}
		if size > capacity.ArtifactBytes.Limit() {
			return nil, capacity.ArtifactBytes.Error()
		}
		for _, old := range r.Artifacts {
			if old.Receipt == a.Receipt && bytes.Equal(old.Data, a.Data) {
				return json.Marshal(map[string]any{"file": old.Name, "receipt": old.Receipt, "caption": old.Caption})
			}
		}
		r.Artifacts = append(r.Artifacts, a)
		return json.Marshal(map[string]any{"file": a.Name, "receipt": a.Receipt, "bytes": len(a.Data), "caption": a.Caption})
	}
	return nil, errors.New("receipt is not in this conversation; call a data operation first")
}
func retainMessageResult(r *automation.Run, result connectors.Result) {
	if result.Receipt.ID == "" {
		return
	}
	for _, old := range r.ChatResults {
		if old.Receipt.ID == result.Receipt.ID {
			return
		}
	}
	raw, err := json.Marshal(result)
	if err != nil || len(raw) > capacity.StateBytes.Limit()/2 {
		return
	}
	r.ChatResults = append(r.ChatResults, result)
	for {
		raw, _ = json.Marshal(r.ChatResults)
		if len(raw) <= capacity.StateBytes.Limit()/2 {
			break
		}
		r.ChatResults = r.ChatResults[1:]
	}
}
