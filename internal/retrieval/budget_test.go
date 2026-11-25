package retrieval

import "testing"

func TestBudgetKeepsRoomAfterRejectedEvidence(t *testing.T) {
	b := NewBudget(3, 1000, 1)
	if !b.Add("first", 300) || b.Add("first", 1) || b.Add("large", 701) {
		t.Fatal("per-file or token allowance was not enforced")
	}
	if !b.Add("second", 300) || !b.Add("third", 400) || b.Tokens() != 1000 {
		t.Fatal("rejected evidence consumed the remaining allowance")
	}
	if b.Add("fourth", 0) {
		t.Fatal("result limit was not enforced")
	}
}

func TestBudgetSupportsAlreadyDiversifiedChatEvidence(t *testing.T) {
	b := NewBudget(0, 0, 0)
	for i := 0; i < DefaultLimit; i++ {
		if !b.Add("", DefaultTokenBudget/DefaultLimit) {
			t.Fatal("chat evidence was diversified a second time")
		}
	}
	if b.Add("", 1) || b.Tokens() != DefaultTokenBudget {
		t.Fatal("default allowance was not enforced")
	}
}
