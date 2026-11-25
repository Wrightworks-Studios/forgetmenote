package web

import (
	"net/http/httptest"
	"net/url"
	"testing"
)

// An operator-chosen recovery key may be weak; guesses share the password limiter.
func TestRecoveryKeyLoginIsRateLimited(t *testing.T) {
	app, _ := fixture(t)
	wrong := url.Values{"token": {"wrong-recovery-key-guess-0123456789abcdef"}}.Encode()
	limited := false
	for i := 0; i < 50 && !limited; i++ {
		w := oidcRequest(app, "POST", "/login", wrong)
		limited = w.Code == 429
		if !limited && w.Code != 401 {
			t.Fatalf("guess %d: %d", i, w.Code)
		}
	}
	if !limited {
		t.Fatal("recovery key guesses were never limited")
	}
}

func TestEmptyRecoveryKeyNeverAuthenticates(t *testing.T) {
	app, _ := fixture(t)
	app.token = ""
	r := httptest.NewRequest("GET", app.origin+"/api/me", nil)
	r.Header.Set("Authorization", "Bearer ")
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatalf("empty bearer matched an empty recovery key: %d", w.Code)
	}
	if w := oidcRequest(app, "POST", "/login", url.Values{"token": {""}}.Encode()); w.Code != 401 {
		t.Fatalf("empty access key signed in: %d", w.Code)
	}
}

// Only failed guesses spend the budget; repeated correct sign-ins never lock out.
func TestCorrectRecoveryKeySignInsDoNotSpendTheGuessBudget(t *testing.T) {
	app, _ := fixture(t)
	right := url.Values{"token": {testToken}}.Encode()
	for i := 0; i < 40; i++ {
		if w := oidcRequest(app, "POST", "/login", right); w.Code != 303 {
			t.Fatalf("correct sign-in %d: %d", i, w.Code)
		}
	}
	wrong := url.Values{"token": {"wrong-recovery-key-guess-0123456789abcdef"}}.Encode()
	for i := 0; i < 50; i++ {
		if w := oidcRequest(app, "POST", "/login", wrong); w.Code == 429 {
			return
		}
	}
	t.Fatal("recovery key guesses were never limited after correct sign-ins")
}
