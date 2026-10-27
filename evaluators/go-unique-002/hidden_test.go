package unique

import (
	"reflect"
	"testing"
)

func TestUniqueRepeated(t *testing.T) {
	got := Unique([]string{"a", "b", "a", "c", "b"})
	want := []string{"a", "b", "c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Unique = %#v; want %#v", got, want)
	}
}

func TestUniquePreservesOrder(t *testing.T) {
	got := Unique([]string{"z", "x", "z", "y", "x"})
	want := []string{"z", "x", "y"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Unique = %#v; want %#v", got, want)
	}
}

func TestUniqueDoesNotModifyInput(t *testing.T) {
	input := []string{"a", "a", "b"}
	before := append([]string(nil), input...)
	result := Unique(input)
	if !reflect.DeepEqual(input, before) {
		t.Fatalf("Unique modified input: got %#v; want %#v", input, before)
	}
	if len(result) == 0 {
		t.Fatal("Unique returned an empty result for non-empty input")
	}
	result[0] = "changed"
	if !reflect.DeepEqual(input, before) {
		t.Fatalf("Unique result shares backing storage with input: got %#v; want %#v", input, before)
	}
}

func TestUniqueEmpty(t *testing.T) {
	got := Unique([]string{})
	if got == nil || len(got) != 0 {
		t.Fatalf("Unique(empty) = %#v; want non-nil empty slice", got)
	}
}
