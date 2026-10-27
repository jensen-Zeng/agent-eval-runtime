package parser

import (
	"errors"
	"testing"
)

// These trusted checks live outside the visible task source and are injected only
// into a clean evaluator workspace by the runtime.
func TestEmpty(t *testing.T) {
	for _, input := range []string{"", " \t\n"} {
		if _, err := ParseCount(input); !errors.Is(err, ErrEmpty) {
			t.Errorf("ParseCount(%q) error = %v; want ErrEmpty", input, err)
		}
	}
}

func TestNegative(t *testing.T) {
	for _, input := range []string{"-1", " -12 "} {
		if _, err := ParseCount(input); !errors.Is(err, ErrNegative) {
			t.Errorf("ParseCount(%q) error = %v; want ErrNegative", input, err)
		}
	}
}

func TestWhitespace(t *testing.T) {
	got, err := ParseCount(" \t23\n")
	if err != nil || got != 23 {
		t.Fatalf("ParseCount(whitespace) = %d, %v; want 23, nil", got, err)
	}
}

func TestRegression(t *testing.T) {
	for input, want := range map[string]int{"0": 0, "7": 7, "007": 7, "214": 214} {
		got, err := ParseCount(input)
		if err != nil || got != want {
			t.Errorf("ParseCount(%q) = %d, %v; want %d, nil", input, got, err, want)
		}
	}
	for _, input := range []string{"12x", "1.5", "999999999999999999999999999999999999"} {
		if _, err := ParseCount(input); err == nil {
			t.Errorf("ParseCount(%q) succeeded; want parse error", input)
		}
	}
}