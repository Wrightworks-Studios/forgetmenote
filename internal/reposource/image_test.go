package reposource

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepositoryImagesArePinnedAndDoNotServeExecutableContent(t *testing.T) {
	f := searchFixture(t)
	image, _ := base64.StdEncoding.DecodeString("R0lGODlhAQABAIAAAAAAAP///yH5BAEAAAAALAAAAAABAAEAAAIBRAA7")
	for name, data := range map[string][]byte{"image.gif": image, "script.png": []byte("<html><script>alert(1)</script></html>")} {
		if err := os.WriteFile(filepath.Join(f.dirs["alpha"], name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	gitRun(t, f.dirs["alpha"], "add", ".")
	gitRun(t, f.dirs["alpha"], "commit", "-m", "media")
	sha := strings.TrimSpace(gitRun(t, f.dirs["alpha"], "rev-parse", "HEAD"))
	f.heads["alpha"].Store(sha)
	state, err := f.m.Refresh(t.Context(), "alpha")
	if err != nil {
		t.Fatal(err)
	}
	got, media, err := f.m.Image(t.Context(), "alpha", state.ID, sha, "image.gif")
	if err != nil || media != "image/gif" || !bytes.Equal(got, image) {
		t.Fatalf("image: %s %v", media, err)
	}
	for _, path := range []string{"script.png", "../image.gif"} {
		if _, _, err = f.m.Image(t.Context(), "alpha", state.ID, sha, path); err == nil {
			t.Fatal("unsafe image", path)
		}
	}
	f.denied.Store(true)
	if _, _, err = f.m.Image(t.Context(), "alpha", state.ID, sha, "image.gif"); err == nil {
		t.Fatal("revoked image read")
	}
}
