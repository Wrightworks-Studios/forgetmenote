package notestore

import "testing"

func TestInstallKeepsTheNewestModelOfTheNewestIncarnation(t *testing.T) {
	s := &Store{models: modelList{limit: 1 << 20}}
	n := &Notebook{s: s, id: "nb"}
	at := func(inc, seq int64) *model { return emptyModel(Head{Seq: seq}, inc) }
	check := func(step string, inc, seq int64) {
		t.Helper()
		if m := n.model.Load(); m == nil || m.inc != inc || m.head.Seq != seq {
			t.Fatalf("%s: installed %+v", step, m)
		}
	}
	n.install(at(1, 4))
	n.install(at(1, 3))
	check("an older revision of the same notebook", 1, 4)
	n.install(at(1, 5))
	check("a newer revision", 1, 5)
	n.install(at(2, 1))
	check("the notebook created again", 2, 1)
	n.install(at(1, 9))
	check("a revision of the deleted notebook", 2, 1)
}

// A model derived by writes accounts the same bytes as one built whole, so
// long edit sessions do not inflate the read models' budget.
func TestDerivedModelsAccountTheirExactSize(t *testing.T) {
	m := emptyModel(Head{Seq: 1}, 1)
	steps := [][]FileChange{
		{{Path: "a.md", Content: []byte("a")}, {Path: "dir/sub/b.md", Content: []byte("b")}, {Path: "dir/c.md", Content: []byte("c")}},
		{{Path: "a.md", Content: []byte("a, edited")}},
		{{Path: "a.md", Content: []byte("a, edited again")}, {Path: "dir/c.md", Content: []byte("c, edited")}},
		{{Path: "dir/sub/renamed-with-a-longer-name.md", From: "dir/sub/b.md"}},
		{{Path: "dir/c.md"}},
		{{Path: "dir/sub/renamed-with-a-longer-name.md"}},
		{{Path: "z.md", Content: []byte("z")}},
	}
	for i, files := range steps {
		prepared := make([]*version, len(files))
		for j, fc := range files {
			if fc.Content != nil {
				prepared[j] = prepare(fc.Path, fc.Content)
			}
		}
		plan, err := planWrite(m, files, prepared)
		if err != nil {
			t.Fatal(i, err)
		}
		m = m.derive(Head{Seq: m.head.Seq + 1}, plan.changed, plan.dirs)
		if m.bytes != m.size() {
			t.Fatalf("step %d: accounted %d bytes, the model has %d", i, m.bytes, m.size())
		}
	}
	if len(m.files) != 2 || len(m.dirs) != 1 {
		t.Fatal(m.paths, len(m.dirs))
	}
}
