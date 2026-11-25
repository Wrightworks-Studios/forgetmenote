package workspace

import (
	"strings"
	"testing"
)

func TestValidNotebookID(t *testing.T) {
	for id, valid := range map[string]bool{
		"default": true, "team-notes-2": true, "0": true, strings.Repeat("a", 64): true,
		"": false, "-lead": false, "Upper": false, "under_score": false, "dot.ted": false,
		"../escape": false, "a/b": false, strings.Repeat("a", 65): false, "trailing\n": false,
	} {
		if ValidNotebookID(id) != valid {
			t.Errorf("ValidNotebookID(%q) = %v", id, !valid)
		}
	}
}
