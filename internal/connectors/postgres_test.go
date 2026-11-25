package connectors

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// FMN_TEST_POSTGRES points only at a disposable database, never production.
func TestPostgresReadBoundary(t *testing.T) {
	endpoint := os.Getenv("FMN_TEST_POSTGRES")
	if endpoint == "" {
		t.Skip("set FMN_TEST_POSTGRES for disposable database integration")
	}
	m, err := New(Config{Version: 1, Connections: []Connection{{ID: "database", Kind: "postgres", Endpoint: endpoint, SecretEnv: "FMN_TEST_POSTGRES_PASSWORD", MaxRows: 2, Operations: []Operation{{Name: "query", ReadOnly: true}, {Name: "schema", ReadOnly: true}}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	call := func(sql string, params []any) (Result, error) {
		raw, _ := json.Marshal(map[string]any{"sql": sql, "params": params})
		return m.Call(context.Background(), "database", "query", raw, "")
	}
	r, err := call("select $1::text as value, current_setting('transaction_read_only') as readonly", []any{"it's bound; not SQL"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(r.Data)
	if !strings.Contains(string(b), "it's bound; not SQL") || !strings.Contains(string(b), "on") || !r.Receipt.Complete {
		t.Fatal(string(b), r.Receipt)
	}
	r, err = call("select generate_series(1,10) as n", []any{})
	if err != nil || r.Receipt.Complete {
		t.Fatal("row limit must be explicit", r, err)
	}
	for _, sql := range []string{"select 1; select 2", "commit", "create table fmn_forbidden(id int)", "with x as (delete from fmn_fixture returning *) select * from x", "select 1 into fmn_forbidden"} {
		if _, err = call(sql, []any{}); err == nil {
			t.Fatal("accepted unsafe statement", sql)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err = m.Call(ctx, "database", "query", json.RawMessage(`{"sql":"select pg_sleep(20)"}`), ""); err == nil {
		t.Fatal("query ignored cancellation")
	}
	if _, err = m.Call(context.Background(), "database", "schema", json.RawMessage(`{}`), ""); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args          string
		rows, columns int
	}{
		{`{"detail":"tables","search":"FMN_FIXTURE"}`, 1, 2},
		{`{"schema":"public","table":"fmn_fixture","detail":"columns","search":"ID"}`, 1, 5},
		{`{"detail":"tables","search":"%"}`, 0, 2},
	} {
		r, err := m.Call(t.Context(), "database", "schema", json.RawMessage(tc.args), "")
		if err != nil {
			t.Fatal(tc.args, err)
		}
		var data struct {
			Columns []any
			Rows    [][]any
		}
		raw, _ := json.Marshal(r.Data)
		if err := json.Unmarshal(raw, &data); err != nil || len(data.Rows) != tc.rows || len(data.Columns) != tc.columns || !r.Receipt.Complete {
			t.Fatalf("focused discovery %s: %s complete=%v error=%v", tc.args, raw, r.Receipt.Complete, err)
		}
	}
}
