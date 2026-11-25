package knowledge

import (
	"forgetmenote/internal/workspace"
	"testing"
)

func TestReviewFingerprintsInvalidateLinkedSources(t *testing.T) {
	snapshot := workspace.Snapshot{Visibility: "visible", Files: []workspace.SnapshotFile{{Path: "notes/claim.md", Hash: "claim", Content: "Claim citing [source](source.md)."}, {Path: "notes/source.md", Hash: "before", Content: "Original"}, {Path: "notes/unrelated.md", Hash: "unrelated", Content: "Independent"}}}
	before := ReviewFingerprints(snapshot)
	snapshot.Files[1].Hash = "after"
	after := ReviewFingerprints(snapshot)
	if before["notes/claim.md"] == after["notes/claim.md"] {
		t.Fatal("changed cited original did not invalidate analysis")
	}
	if before["notes/unrelated.md"] != after["notes/unrelated.md"] {
		t.Fatal("unrelated source change invalidated independent analysis")
	}
	snapshot.Visibility = "changed-exclusions"
	latest := ReviewFingerprints(snapshot)
	if latest["notes/claim.md"] == after["notes/claim.md"] {
		t.Fatal("visibility change retained cached analysis")
	}
}
