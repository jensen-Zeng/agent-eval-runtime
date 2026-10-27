package parser

import "testing"

func TestVisibleValidCount(t *testing.T) {
	got, err := ParseCount("42")
	if err != nil || got != 42 {
		t.Fatalf("ParseCount(42) = %d, %v; want 42, nil", got, err)
	}
}