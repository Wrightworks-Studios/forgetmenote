package filestate

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"forgetmenote/internal/blob"
	"forgetmenote/internal/blob/blobtest"
)

func TestBlobFilesMoveToBlobStorageAndBack(t *testing.T) {
	ctx := context.Background()
	data := t.TempDir()
	os.MkdirAll(filepath.Join(data, "source-jobs", "0123"), 0700)
	os.WriteFile(filepath.Join(data, "source-jobs", "0123", "original"), []byte("original bytes"), 0600)
	src, err := OpenSource(data)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	// While blob storage is the data directory the files are already in it.
	var here Tally
	if err = src.ImportBlob(ctx, &here, "source-jobs/0123/original"); err != nil {
		t.Fatal(err)
	}
	s3 := blobtest.Start(t)
	src.Blobs = s3.Store(t, "instance")
	var imported Tally
	if err = src.ImportBlob(ctx, &imported, "source-jobs/0123/original"); err != nil {
		t.Fatal(err)
	}
	if objects := s3.Objects(); len(objects) != 1 || string(objects["instance/source-jobs/0123/original"]) != "original bytes" {
		t.Fatalf("stored objects: %q", objects)
	}
	if here.Sum() != imported.Sum() || imported.Sum().Files != 1 {
		t.Fatalf("the report depends on where the blob went: %+v %+v", here.Sum(), imported.Sum())
	}
	if err = src.ImportBlob(ctx, &imported, "source-jobs/0123"); err == nil {
		t.Fatal("imported a directory as a blob")
	}

	out, err := CreateTarget(filepath.Join(t.TempDir(), "export"))
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	out.Blobs = src.Blobs
	var exported Tally
	if err = out.ExportBlob(ctx, &exported, "source-jobs/0123/original"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(out.root.Name(), "source-jobs", "0123", "original"))
	if err != nil || string(raw) != "original bytes" || exported.Sum() != imported.Sum() {
		t.Fatalf("exported blob: %q %v %+v", raw, err, exported.Sum())
	}
	if err = out.ExportBlob(ctx, &exported, "source-jobs/0123/original"); err == nil {
		t.Fatal("an export replaced a file")
	}
	blob.Write(ctx, out.Blobs, "source-jobs/0124/original", []byte("x"))
	if err = out.ExportBlob(ctx, &exported, "source-jobs/0125/original"); err == nil {
		t.Fatal("exported a missing blob")
	}
}
