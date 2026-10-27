package task

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestFrozenFixtureManifestsLoad(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		id    string
		tests []string
	}{
		{
			id:    "go-parser-001",
			tests: []string{"TestEmpty", "TestNegative", "TestWhitespace", "TestRegression"},
		},
		{
			id:    "go-unique-002",
			tests: []string{"TestUniqueSingle", "TestUniqueRepeated", "TestUniquePreservesOrder", "TestUniqueDoesNotModifyInput", "TestUniqueEmpty"},
		},
		{
			id:    "go-range-003",
			tests: []string{"TestClampInsideRange", "TestClampLowerBound", "TestClampUpperBound", "TestClampInvalidRange", "TestClampInclusiveBounds", "TestClampNegative"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			manifest := filepath.Join(root, "tasks", "fixtures", tc.id, "task.json")
			loaded, err := Load(manifest, root)
			if err != nil {
				t.Fatalf("Load(%s): %v", tc.id, err)
			}
			if loaded.ID != tc.id {
				t.Fatalf("loaded task id = %q; want %q", loaded.ID, tc.id)
			}
			if !reflect.DeepEqual(loaded.RequiredTests, tc.tests) {
				t.Fatalf("required tests = %#v; want %#v", loaded.RequiredTests, tc.tests)
			}
			if len(loaded.Source) == 0 || len(loaded.Evaluator) == 0 {
				t.Fatal("loaded task has an empty source or evaluator bundle")
			}
		})
	}
}
