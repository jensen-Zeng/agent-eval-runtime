package rangeclamp

import "testing"

func TestClampInsideRange(t *testing.T) {
	got, err := Clamp(5, 1, 9)
	if err != nil || got != 5 {
		t.Fatalf("Clamp(5, 1, 9) = %d, %v; want 5, nil", got, err)
	}
}
