package search

import (
	"forgetmenote/internal/catalog"
	"forgetmenote/internal/knowledge"
	"forgetmenote/internal/pg/pgtest"
	"reflect"
	"strings"
	"testing"
)

func TestWritingProjectionCountsTowardIdleByteBudget(t *testing.T) {
	t.Setenv("FMN_LIMIT_IDLE_INDEX_BYTES", "65536")
	db := pgtest.Org(t)
	projects, err := catalog.Open(t.TempDir(), nil, db)
	if err != nil {
		t.Fatal(err)
	}
	defer projects.Close()
	store, err := projects.Access(catalog.Bootstrap, "default", "read")
	if err != nil {
		t.Fatal(err)
	}
	indexes := NewIndexes(db, projects, nil)
	defer indexes.Close()
	engine, release, err := indexes.Acquire(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	analysis := engine.Writing([]byte(strings.Repeat("# x\n\n", 2000)))
	sectionsBytes := len(analysis.Sections) * int(reflect.TypeOf(knowledge.WritingSection{}).Size())
	if sectionsBytes <= 65536 {
		release()
		t.Fatalf("fixture did not retain an over-budget section array: %d", sectionsBytes)
	}
	release()
	again, release, err := indexes.Acquire(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if again == engine {
		t.Fatalf("idle engine retained at least %d bytes in writing sections alone under a 65536-byte budget (estimate=%d)", sectionsBytes, engine.EstimatedBytes())
	}
}

func TestSnapshotMetadataCountsTowardIdleByteBudget(t *testing.T) {
	t.Setenv("FMN_LIMIT_IDLE_INDEX_BYTES", "98304")
	db := pgtest.Org(t)
	projects, err := catalog.Open(t.TempDir(), nil, db)
	if err != nil {
		t.Fatal(err)
	}
	defer projects.Close()
	store, err := projects.Access(catalog.Bootstrap, "default", "edit")
	if err != nil {
		t.Fatal(err)
	}
	content := "---\ntags: [" + strings.TrimSuffix(strings.Repeat("x,", 10000), ",") + "]\n---\n# Tags\nSynthetic metadata evidence.\n"
	if _, err = store.Save(t.Context(), "tagged.md", []byte(content), "", ""); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Snapshot(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	tagsBytes := 0
	for _, file := range snapshot.Files {
		if file.Path == "tagged.md" {
			tagsBytes = len(file.Metadata.Tags) * int(reflect.TypeOf("").Size())
		}
	}
	if tagsBytes <= 98304 {
		t.Fatalf("fixture did not create an over-budget metadata slice: %d", tagsBytes)
	}
	indexes := NewIndexes(db, projects, nil)
	defer indexes.Close()
	engine, release, err := indexes.Acquire(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	_, err = engine.Configure(t.Context(), knowledge.Config{Enabled: true, Model: "synthetic", Dimensions: 3, PassageRunes: 1400, Splitter: "markdown-heading-v1"}, false)
	if err != nil {
		release()
		t.Fatal(err)
	}
	release()
	again, release, err := indexes.Acquire(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if again == engine {
		t.Fatalf("idle engine retained at least %d bytes in snapshot tag headers alone under a 98304-byte budget (estimate=%d)", tagsBytes, engine.EstimatedBytes())
	}
}
