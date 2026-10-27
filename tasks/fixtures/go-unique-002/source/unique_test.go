package unique

import "testing"

func TestUniqueSingle(t *testing.T) {
	got := Unique([]string{"only"})
	if len(got) != 1 || got[0] != "only" {
		t.Fatalf("Unique(single) = %#v; want [only]", got)
	}
}
