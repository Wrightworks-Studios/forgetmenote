package githubapp

import (
	"crypto/rand"
	"crypto/rsa"
	"strings"
	"testing"
)

func TestSealedTokensOpenOnlyWithTheSameAppKey(t *testing.T) {
	key := func() *rsa.PrivateKey {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	gateway, other := &Client{key: key()}, &Client{key: key()}
	sealed, err := gateway.SealToken("ghs_synthetic-installation-token")
	if err != nil || strings.Contains(sealed, "synthetic") {
		t.Fatalf("seal = %q %v", sealed, err)
	}
	again, _ := gateway.SealToken("ghs_synthetic-installation-token")
	if again == sealed {
		t.Fatal("sealing is deterministic")
	}
	if token, err := (&Client{key: gateway.key}).OpenToken(sealed); err != nil || token != "ghs_synthetic-installation-token" {
		t.Fatalf("another gateway with the App key cannot open it: %q %v", token, err)
	}
	if _, err := other.OpenToken(sealed); err == nil {
		t.Fatal("a different App key opened the token")
	}
	if _, err := gateway.OpenToken(sealed[:len(sealed)-2] + "AA"); err == nil {
		t.Fatal("a damaged seal opened")
	}
}
