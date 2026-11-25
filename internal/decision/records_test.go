package decision

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"forgetmenote/internal/pg"
	"forgetmenote/internal/pg/pgtest"
)

type tally struct {
	Count int            `json:"count"`
	By    map[string]int `json:"by"`
}

// Read-modify-write cycles of one record from two gateways at once all land,
// starting from a record neither has written.
func TestRecordUpdatesOnTwoGatewaysLoseNothing(t *testing.T) {
	db := pgtest.Org(t)
	other, _ := pgtest.Gateway(t, db)
	var wg sync.WaitGroup
	failures := make(chan error, 40)
	for gateway, handle := range map[string]*pg.DB{"one": db, "two": other} {
		records, err := OpenRecords(handle)
		if err != nil {
			t.Fatal(err)
		}
		for range 20 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				value := tally{By: map[string]int{}}
				failures <- records.Update("tally.json", &value, func(bool) error {
					value.Count++
					value.By[gateway]++
					return nil
				})
			}()
		}
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	records, _ := OpenRecords(db)
	var value tally
	if err := records.Read("tally.json", &value); err != nil || value.Count != 40 || value.By["one"] != 20 || value.By["two"] != 20 {
		t.Fatalf("concurrent updates were lost: %+v %v", value, err)
	}
}

// An update writes only a change: the default stands in for a missing record
// and is not written unchanged, a stored record replaces the default, a
// refused change keeps the record and returns the refusal, and an unchanged
// record is not written again.
func TestRecordUpdateWritesOnlyAChange(t *testing.T) {
	db := pgtest.Org(t)
	records, err := OpenRecords(db)
	if err != nil {
		t.Fatal(err)
	}
	value := tally{By: map[string]int{"default": 1}}
	if err = records.Update("tally.json", &value, func(found bool) error {
		if found {
			t.Error("a missing record was found")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err = records.Read("tally.json", &value); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("an unchanged default was written", err)
	}
	value = tally{By: map[string]int{"default": 1}}
	if err = records.Update("tally.json", &value, func(bool) error { value = tally{Count: 1, By: map[string]int{"stored": 1}}; return nil }); err != nil {
		t.Fatal(err)
	}
	value = tally{By: map[string]int{"default": 1}}
	if err = records.Update("tally.json", &value, func(found bool) error {
		if !found || value.By["default"] != 0 || value.By["stored"] != 1 {
			t.Errorf("the stored record did not replace the default: %v %+v", found, value)
		}
		value.Count++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	refusal := errors.New("refused")
	if err = records.Update("tally.json", &value, func(bool) error { value.Count = 100; return refusal }); err != refusal {
		t.Fatal("the refusal was not returned as it is", err)
	}
	restore := pgtest.FailTable(t, db, "jev_records")
	if err = records.Update("tally.json", &value, func(bool) error { return nil }); err != nil {
		t.Fatal("an unchanged record was written", err)
	}
	if err = records.Update("tally.json", &value, func(bool) error { value.Count++; return nil }); !errors.Is(err, ErrStorage) {
		t.Fatal("a failed write was not reported", err)
	}
	restore()
	if err = records.Read("tally.json", &value); err != nil || value.Count != 2 || value.By["stored"] != 1 {
		t.Fatalf("the record changed: %+v %v", value, err)
	}
	records.Bytes = 8
	if err = records.Update("tally.json", &value, func(bool) error { return nil }); !errors.Is(err, ErrStorage) {
		t.Fatal("a record past the limit was read", err)
	}
}

type listed struct {
	State string `json:"state"`
	N     int    `json:"n"`
}

// Listing bodies reads names and bodies in name order with one query, reports
// that more exist past the limit, keeps only matching bodies when asked, and
// lists a body past the record limit without decoding it.
func TestRecordListBodiesReadsMatchingBodiesInOrder(t *testing.T) {
	db := pgtest.Org(t)
	records, err := OpenRecords(db)
	if err != nil {
		t.Fatal(err)
	}
	for i, state := range []string{"queued", "completed", "queued", "paused"} {
		if err = records.Write(fmt.Sprintf("jobs/%d.json", i), listed{State: state, N: i}); err != nil {
			t.Fatal(err)
		}
	}
	if err = records.Write("other/0.json", listed{State: "queued"}); err != nil {
		t.Fatal(err)
	}
	if err = records.Write("other/1.json", []string{"not", "an", "object"}); err != nil {
		t.Fatal(err)
	}
	if others, err := records.ListBodies("other/", 10); err != nil || len(others) != 2 {
		t.Fatalf("listing without a match skipped a body: %+v %v", others, err)
	}
	bodies, err := records.ListBodies("jobs/", 2)
	if err != nil || len(bodies) != 3 {
		t.Fatalf("listing did not stop one past the limit: %+v %v", bodies, err)
	}
	for i, body := range bodies {
		var value listed
		if body.Name != fmt.Sprintf("jobs/%d.json", i) || body.Decode(&value) != nil || value.N != i {
			t.Fatalf("body %d is not the stored record in order: %s %+v", i, body.Name, value)
		}
	}
	queued, err := records.ListMatching("jobs/", map[string]any{"state": "queued"}, 10)
	if err != nil || len(queued) != 2 || queued[0].Name != "jobs/0.json" || queued[1].Name != "jobs/2.json" {
		t.Fatalf("matching listed other bodies: %+v %v", queued, err)
	}
	if none, err := records.ListMatching("jobs/", map[string]any{"state": "failed"}, 10); err != nil || len(none) != 0 {
		t.Fatalf("nothing matches, yet: %+v %v", none, err)
	}
	records.Bytes = 8
	bodies, err = records.ListBodies("jobs/", 10)
	if err != nil || len(bodies) != 4 {
		t.Fatalf("a body past the limit hid the listing: %+v %v", bodies, err)
	}
	if err = bodies[0].Decode(new(listed)); !errors.Is(err, ErrStorage) {
		t.Fatal("a body past the limit was decoded", err)
	}
}

// Removing a record removes what it owns in the same transaction, and nothing
// else; an empty owned prefix, which would own everything, is refused.
func TestRecordRemoveTakesOwnedRecords(t *testing.T) {
	db := pgtest.Org(t)
	records, err := OpenRecords(db)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"jobs/a.json", "jobs/b.json", "results/a-00000000.json", "results/a-00000001.json", "results/b-00000000.json"} {
		if err = records.Write(name, listed{}); err != nil {
			t.Fatal(err)
		}
	}
	if err = records.Remove("jobs/a.json", ""); !errors.Is(err, ErrStorage) {
		t.Fatal("an empty owned prefix was accepted", err)
	}
	if err = records.Remove("jobs/a.json", "results/a-"); err != nil {
		t.Fatal(err)
	}
	names, err := records.List("", 10)
	if err != nil || strings.Join(names, " ") != "jobs/b.json results/b-00000000.json" {
		t.Fatalf("removal kept owned records or took others: %v %v", names, err)
	}
	if err = records.Remove("jobs/b.json"); err != nil {
		t.Fatal(err)
	}
	if names, _ = records.List("", 10); strings.Join(names, " ") != "results/b-00000000.json" {
		t.Fatalf("a plain removal took more than its record: %v", names)
	}
}
