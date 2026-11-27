package pg_test

import (
	"testing"

	"forgetmenote/internal/pg/pgtest"
)

// Each gateway's record of a kind is listed while the gateway is present. A
// gateway that has stopped drops out at once, and the next gateway that
// records removes what it left, so records do not pile up across restarts.
func TestGatewayRecordsAreThoseOfLiveGateways(t *testing.T) {
	a := pgtest.Org(t)
	b, leave := pgtest.Gateway(t, a)
	ctx := t.Context()
	if err := a.RecordGateway(ctx, "settings", []byte(`{"off":[]}`)); err != nil {
		t.Fatal(err)
	}
	if err := b.RecordGateway(ctx, "settings", []byte(`{"off":["chat"]}`)); err != nil {
		t.Fatal(err)
	}
	if err := b.RecordGateway(ctx, "settings", []byte(`{"off":["memory"]}`)); err != nil {
		t.Fatal(err)
	}
	if err := a.RecordGateway(ctx, "other", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	records, err := a.GatewayRecords(ctx, "settings")
	if err != nil || len(records) != 2 || string(records[a.Holder]) != `{"off": []}` || string(records[b.Holder]) != `{"off": ["memory"]}` {
		t.Fatalf("records = %q, %v", records, err)
	}

	leave()
	if records, err = a.GatewayRecords(ctx, "settings"); err != nil || len(records) != 1 || records[a.Holder] == nil {
		t.Fatalf("after the other gateway stopped, records = %q, %v", records, err)
	}
	stored := func() (n int) {
		if err := a.Pool.QueryRow(ctx, `SELECT count(*) FROM documents WHERE org = $1 AND kind = 'settings'`, a.Org).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := stored(); n != 2 {
		t.Fatalf("the stopped gateway's record was removed before another gateway recorded: %d rows", n)
	}
	c, _ := pgtest.Gateway(t, a)
	if err := c.RecordGateway(ctx, "settings", []byte(`{"off":[]}`)); err != nil {
		t.Fatal(err)
	}
	if n := stored(); n != 2 {
		t.Fatalf("records of a stopped gateway stay: %d rows", n)
	}
}
