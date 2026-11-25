package sources

import (
	"os"
	"strings"
)

// The image records the actual pinned build inputs. Native installations retain
// a visible unknown provenance rather than claiming the Docker data revision.
func sourceExtractor(name string) string {
	raw, err := os.ReadFile("/usr/share/forgetmenote/source-versions")
	if err != nil || len(raw) > 1024 || strings.ContainsAny(string(raw), "\x00\r\n") {
		return name + "; installed engine/data revisions unavailable"
	}
	return name + "; " + strings.TrimSpace(string(raw))
}
