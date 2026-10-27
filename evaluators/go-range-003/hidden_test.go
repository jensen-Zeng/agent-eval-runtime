package rangeclamp

import (
	"errors"
	"testing"
)

func TestClampLowerBound(t *testing.T) {
	got, err := Clamp(-10, -3, 8)
	if err != nil || got != -3 {
		t.Fatalf("Clamp lower = %d, %v; want -3, nil", got, err)
	}
}

func TestClampUpperBound(t *testing.T) {
	got, err := Clamp(20, -3, 8)
	if err != nil || got != 8 {
		t.Fatalf("Clamp upper = %d, %v; want 8, nil", got, err)
	}
}

func TestClampInvalidRange(t *testing.T) {
	got, err := Clamp(2, 9, 1)
	if got != 0 || !errors.Is(err, ErrRange) {
		t.Fatalf("Clamp invalid = %d, %v; want 0, ErrRange", got, err)
	}
}

func TestClampInclusiveBounds(t *testing.T) {
	for _, value := range []int{-3, 8} {
		got, err := Clamp(value, -3, 8)
		if err != nil || got != value {
			t.Errorf("Clamp(%d, -3, 8) = %d, %v; want %d, nil", value, got, err, value)
		}
	}
}

func TestClampNegative(t *testing.T) {
	got, err := Clamp(-5, -10, -2)
	if err != nil || got != -5 {
		t.Fatalf("Clamp negative = %d, %v; want -5, nil", got, err)
	}
}
