package calculator

import "testing"

func TestAdd(t *testing.T) {
	if got := Add(4, 5); got != 9 {
		t.Fatalf("Add(4, 5) = %d, want 9", got)
	}
	if got := Add(-3, 7); got != 4 {
		t.Fatalf("Add(-3, 7) = %d, want 4", got)
	}
}
