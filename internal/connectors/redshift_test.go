//go:build !fmn_omit_redshift

package connectors

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRedshiftDataAPIExecutionBoundsAndCancellation(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "synthetic-access-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "synthetic-secret-key")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	var executes, pages, cancels atomic.Int32
	var running atomic.Bool
	var failure atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256") {
			t.Error("AWS request was not signed")
		}
		var input map[string]any
		json.NewDecoder(r.Body).Decode(&input)
		switch {
		case strings.HasSuffix(r.Header.Get("X-Amz-Target"), ".ExecuteStatement"):
			executes.Add(1)
			if input["Database"] != "analytics" || input["WorkgroupName"] != "readonly" || !strings.Contains(input["Sql"].(string), "LIMIT 3") || input["ClientToken"] == "" {
				t.Errorf("unpinned execution: %#v", input)
			}
			if strings.Contains(input["Sql"].(string), "user-value") {
				t.Error("parameter interpolated into SQL")
			}
			json.NewEncoder(w).Encode(map[string]any{"Id": "statement-one"})
		case strings.HasSuffix(r.Header.Get("X-Amz-Target"), ".DescribeStatement"):
			status := "FINISHED"
			if running.Load() {
				status = "STARTED"
			}
			message, _ := failure.Load().(string)
			if message != "" {
				status = "FAILED"
			}
			json.NewEncoder(w).Encode(map[string]any{"Id": "statement-one", "Status": status, "HasResultSet": true, "Error": message})
		case strings.HasSuffix(r.Header.Get("X-Amz-Target"), ".GetStatementResult"):
			pages.Add(1)
			if input["NextToken"] == nil {
				w.Write([]byte(`{"ColumnMetadata":[{"name":"count","typeName":"int8"}],"Records":[[{"longValue":1}]],"NextToken":"next","TotalNumRows":3}`))
			} else {
				w.Write([]byte(`{"ColumnMetadata":[{"name":"count","typeName":"int8"}],"Records":[[{"longValue":2}],[{"longValue":3}]],"TotalNumRows":3}`))
			}
		case strings.HasSuffix(r.Header.Get("X-Amz-Target"), ".CancelStatement"):
			cancels.Add(1)
			w.Write([]byte(`{"Status":true}`))
		default:
			t.Errorf("unexpected target %s", r.Header.Get("X-Amz-Target"))
			w.WriteHeader(400)
		}
	}))
	defer upstream.Close()
	var cfg Config
	raw := `{"version":1,"connections":[{"id":"warehouse","kind":"redshift","endpoint":"` + upstream.URL + `","maxRows":2,"redshift":{"region":"us-east-1","database":"analytics","workgroup":"readonly"},"operations":[{"name":"query","readOnly":true},{"name":"schema","readOnly":true}]}]}`
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}
	m, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	result, err := m.Call(t.Context(), "warehouse", "query", json.RawMessage(`{"sql":"select :value","params":{"value":"user-value"}}`), "")
	if err != nil {
		t.Fatal(err)
	}
	if result.Receipt.Complete || pages.Load() != 2 || len(result.Data.(map[string]any)["rows"].([]any)) != 2 {
		t.Fatalf("lost result bounds: %+v, pages=%d", result, pages.Load())
	}
	for _, sql := range []string{"delete from accounts", "select 1; drop table accounts", "with deleted as (delete from accounts returning *) select * from deleted", "select 1 /* unterminated"} {
		args, _ := json.Marshal(map[string]any{"sql": sql})
		if _, err = m.Call(t.Context(), "warehouse", "query", args, ""); !errors.Is(err, ErrQuery) {
			t.Fatalf("unsafe SQL accepted: %s: %v", sql, err)
		}
	}
	if executes.Load() != 1 {
		t.Fatal("unsafe statement reached AWS")
	}
	running.Store(true)
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()
	_, err = m.Call(ctx, "warehouse", "query", json.RawMessage(`{"sql":"select 1"}`), "")
	if !errors.Is(err, context.DeadlineExceeded) || cancels.Load() != 1 {
		t.Fatalf("remote execution not canceled: %v, cancels=%d", err, cancels.Load())
	}
	running.Store(false)
	for _, tc := range []struct {
		message    string
		repairable bool
	}{{`ERROR: column "private_column" does not exist`, true}, {"ERROR: permission denied for private_table", false}, {"ERROR: internal resource unavailable", false}} {
		failure.Store(tc.message)
		_, err = m.Call(t.Context(), "warehouse", "query", json.RawMessage(`{"sql":"select 1"}`), "")
		if err == nil || errors.Is(err, ErrQuery) != tc.repairable || strings.Contains(err.Error(), "private_") {
			t.Fatalf("incorrect/redacted Redshift failure classification: %v", err)
		}
	}
}

func TestRedshiftSourcePolicyBeforeExecution(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "synthetic-access-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "synthetic-secret-key")
	var executes atomic.Int32
	var lastSQL string
	var lastParams []struct{ Name, Value string }
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.Header.Get("X-Amz-Target"), ".ExecuteStatement"):
			executes.Add(1)
			var input struct {
				SQL        string `json:"Sql"`
				Parameters []struct{ Name, Value string }
			}
			json.NewDecoder(r.Body).Decode(&input)
			lastSQL = input.SQL
			lastParams = input.Parameters
			w.Write([]byte(`{"Id":"one"}`))
		case strings.HasSuffix(r.Header.Get("X-Amz-Target"), ".DescribeStatement"):
			w.Write([]byte(`{"Status":"FINISHED","HasResultSet":true}`))
		case strings.HasSuffix(r.Header.Get("X-Amz-Target"), ".GetStatementResult"):
			w.Write([]byte(`{"ColumnMetadata":[{"name":"id","typeName":"int8"}],"Records":[[{"longValue":1}],[{"longValue":2}],[{"longValue":3}]]}`))
		default:
			w.WriteHeader(400)
		}
	}))
	defer upstream.Close()
	var cfg Config
	json.Unmarshal([]byte(`{"version":1,"connections":[{"id":"warehouse","kind":"redshift","endpoint":`+strconvJSON(upstream.URL)+`,"redshift":{"region":"us-east-1","database":"analytics","workgroup":"readonly","schemas":["src_app","public"],"deniedColumns":["email","phone"],"maxCells":2},"operations":[{"name":"query","readOnly":true},{"name":"schema","readOnly":true}]}]}`), &cfg)
	m, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	for _, sql := range []string{
		`select id from private.accounts`, `select id from other.public.accounts`,
		`select "email" from src_app.accounts`, `select lower(a.phone) from public.accounts a`,
		`select a.* from public.accounts a`, `select * from public.accounts`,
		`select top 10 * from public.accounts`,
		`select id from public.accounts a, private.accounts b`,
		`select id from accounts`, `select id from (private.accounts) a`,
		`with x as (table private.accounts) select id from x`,
		`select id from public.accounts union table private.accounts`,
		`select id from public.some_function()`,
		`select (with accounts as (select id from public.accounts) select id from accounts) from accounts`,
	} {
		args, _ := json.Marshal(map[string]any{"sql": sql})
		if _, err = m.Call(t.Context(), "warehouse", "query", args, ""); err == nil {
			t.Fatalf("policy accepted %s", sql)
		}
	}
	if executes.Load() != 0 {
		t.Fatal("rejected query reached AWS")
	}
	for _, sql := range []string{
		`select count(*) from src_app.accounts`,
		`select 'email' as label, id from "public"."accounts"`,
		`with totals as (select id from public.accounts), next as (select id from totals) select id from next`,
		`select a.id * 2 from public.accounts a where exists (select 1 from src_app.accounts b where a.id=b.id)`,
	} {
		args, _ := json.Marshal(map[string]any{"sql": sql})
		result, err := m.Call(t.Context(), "warehouse", "query", args, "")
		if err != nil {
			t.Fatalf("allowed query: %s: %v", sql, err)
		}
		if result.Receipt.Complete || len(result.Data.(map[string]any)["rows"].([]any)) != 2 {
			t.Fatal("cell limit not applied")
		}
	}
	before := executes.Load()
	if _, err = m.Call(t.Context(), "warehouse", "schema", json.RawMessage(`{"schema":"private"}`), ""); err == nil || executes.Load() != before {
		t.Fatal("schema policy not enforced")
	}
	if !errors.Is(err, ErrQuery) || !strings.Contains(err.Error(), "src_app, public") || !strings.Contains(err.Error(), "omit schema") {
		t.Fatalf("schema discovery did not explain how to recover within policy: %v", err)
	}
	if _, err = m.Call(t.Context(), "warehouse", "schema", json.RawMessage(`{"table":"accounts"}`), ""); err != nil || !strings.Contains(lastSQL, "table_name=:table") || !strings.Contains(lastSQL, "table_schema IN") {
		t.Fatal("table discovery without a guessed schema must keep policy filters", err, lastSQL)
	}
	if _, err = m.Call(t.Context(), "warehouse", "schema", json.RawMessage(`{}`), ""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(lastSQL, "table_schema IN") || !strings.Contains(lastSQL, "column_name NOT IN") {
		t.Fatal("schema discovery omitted policy filters")
	}
	for _, detail := range []string{"tables", "columns"} {
		args, _ := json.Marshal(map[string]string{"detail": detail, "search": "Revenue'); DROP TABLE accounts;--"})
		if _, err = m.Call(t.Context(), "warehouse", "schema", args, ""); err != nil {
			t.Fatalf("focused %s discovery failed: %v", detail, err)
		}
		if !strings.Contains(lastSQL, "table_schema IN") || !strings.Contains(lastSQL, "column_name NOT IN") || strings.Contains(lastSQL, "DROP TABLE") {
			t.Fatal("focused discovery lost policy filters or interpolated search", lastSQL)
		}
		bound := false
		for _, p := range lastParams {
			bound = bound || p.Name == "search" && p.Value == "Revenue'); DROP TABLE accounts;--"
		}
		if !bound || !strings.Contains(lastSQL, "lower(:search)") {
			t.Fatal("case-insensitive search was not bound", lastSQL)
		}
		if detail == "tables" && (!strings.Contains(lastSQL, "SELECT DISTINCT table_schema,table_name FROM") || strings.Contains(lastSQL, "ordinal_position")) {
			t.Fatal("table discovery still returns every column", lastSQL)
		}
		if detail == "columns" && !strings.Contains(lastSQL, "column_name,data_type,is_nullable") {
			t.Fatal("column discovery lost its column metadata", lastSQL)
		}
	}
}
