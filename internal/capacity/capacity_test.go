package capacity

import "testing"

func TestOperatorBudgetsValidateAndOverride(t *testing.T) {
	t.Setenv(IndexBytes.Env(), "2147483648")
	if err := Validate(); err != nil {
		t.Fatal(err)
	}
	if IndexBytes.Limit() != 2<<30 {
		t.Fatal("operator override ignored")
	}
	for _, invalid := range []string{"0", "-1", "invalid", "9223372036854775807"} {
		t.Setenv(IndexBytes.Env(), invalid)
		if Validate() == nil {
			t.Fatalf("accepted %q", invalid)
		}
	}
}

func TestUnknownBudgetFailsStartup(t *testing.T) {
	t.Setenv("FMN_LIMIT_INDX_BYTES", "1024")
	if Validate() == nil {
		t.Fatal("silently ignored a misspelled resource budget")
	}
}

func TestRequestConcurrencyBudgetsAreKnownOperatorControls(t *testing.T) {
	for _, name := range []string{"FMN_LIMIT_READ_REQUESTS", "FMN_LIMIT_WRITE_REQUESTS", "FMN_LIMIT_SOURCE_REQUESTS", "FMN_LIMIT_DECODE_REQUESTS"} {
		t.Setenv(name, "2")
	}
	if err := Validate(); err != nil {
		t.Fatal(err)
	}
}
