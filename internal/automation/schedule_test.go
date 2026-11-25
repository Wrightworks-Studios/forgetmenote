package automation

import (
	"forgetmenote/internal/pg/pgtest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCalendarDefinitionSchedulesOnceAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "config.json")
	if err := os.WriteFile(file, []byte(`{"version":2,"jobs":[{"id":"weekday","kind":"digest","actor":"robot","project":"notes","output":"report.md","calls":[{"connection":"metrics","operation":"read"}],"schedule":{"timezone":"America/Denver","weekdays":[1,2,3,4,5],"hours":[9],"minutes":[0],"catchUpSeconds":3600}}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	config, err := Load(file)
	if err != nil {
		t.Fatal("calendar config", err)
	}
	q, err := Open(pgtest.Dir(t, filepath.Join(dir, "runs")))
	if err != nil {
		t.Fatal(err)
	}
	// 09:30 MDT, inside the catch-up window.
	now := time.Date(2026, 10, 6, 15, 30, 0, 0, time.UTC)
	if err = q.Schedule(config.Jobs, now); err != nil {
		t.Fatal(err)
	}
	runs, err := q.List()
	if err != nil || len(runs) != 1 {
		t.Fatal("scheduled runs", len(runs), err)
	}
	if err = q.Cancel(runs[0].ID); err != nil {
		t.Fatal(err)
	}
	q, err = Open(pgtest.Dir(t, filepath.Join(dir, "runs")))
	if err != nil {
		t.Fatal(err)
	}
	if err = q.Schedule(config.Jobs, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	runs, err = q.List()
	if err != nil || len(runs) != 1 {
		t.Fatal("duplicated calendar slot", len(runs), err)
	}
	if err = q.Schedule(config.Jobs, now.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	runs, err = q.List()
	if err != nil || len(runs) != 2 {
		t.Fatal("next day", len(runs), err)
	}
}
