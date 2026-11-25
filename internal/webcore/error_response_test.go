package webcore

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAPIErrorAdapterPreservesStreamingAndJSONContracts(t *testing.T) {
	t.Run("stream", func(t *testing.T) {
		response := httptest.NewRecorder()
		writer := JSONErrors(response, false)
		writer.Header().Set("Content-Type", "text/event-stream")
		if err := http.NewResponseController(writer).Flush(); err != nil {
			t.Fatal(err)
		}
		if !response.Flushed {
			t.Fatal("stream was buffered")
		}
		// Once flushed, subsequent error status cannot replace the committed200.
		http.Error(writer, "late stream text", 500)
		writer.Finish()
		if response.Code != 200 || response.Body.String() != "late stream text\n" {
			t.Fatal("stream response changed", response.Code, response.Body)
		}
	})
	t.Run("action JSON", func(t *testing.T) {
		response := httptest.NewRecorder()
		writer := JSONErrors(response, true)
		WriteError(writer, Error{Status: 403, Code: "read_only", Message: "read only"}, true)
		writer.Finish()
		var body struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if json.Unmarshal(response.Body.Bytes(), &body) != nil || body.Error.Code != "read_only" || response.Code != 403 {
			t.Fatal("existing action JSON changed", response.Code, response.Body)
		}
	})
	t.Run("plain rejection", func(t *testing.T) {
		response := httptest.NewRecorder()
		writer := JSONErrors(response, false)
		http.Error(writer, "Denied", 403)
		writer.Finish()
		var body struct{ Code, Message string }
		if json.Unmarshal(response.Body.Bytes(), &body) != nil || body.Code != "forbidden" || body.Message != "Denied" || response.Code != 403 {
			t.Fatal("plain rejection not normalized", response.Code, response.Body)
		}
	})
}
