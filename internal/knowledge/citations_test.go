package knowledge

import "testing"

func TestCitationTablesStayUnchecked(t *testing.T) {
	claims := CitationClaims("| Claim | Source |\n| --- | --- |\n| Cooling required | [Process](/note.md) |")
	if len(claims) != 1 || claims[0].Mapped {
		t.Fatalf("guessed table association: %+v", claims)
	}
}
