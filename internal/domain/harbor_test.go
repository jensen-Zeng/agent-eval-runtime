package domain

import (
	"strings"
	"testing"
)

func TestHarborEnvironmentDefaultsDoNotDuplicateDeclaredKeys(t *testing.T) {
	task := Task{Backend: "harbor", Harbor: &HarborSpec{AgentEnv: []string{"PYTHONDONTWRITEBYTECODE=1", "PYTHONUNBUFFERED=1", "TZ=Asia/Shanghai"}}}
	seen := map[string]string{}
	for _, item := range task.CommandEnvironment() {
		k, v, ok := strings.Cut(item, "=")
		if !ok {
			t.Fatal(item)
		}
		if _, exists := seen[k]; exists {
			t.Fatalf("duplicate key: %s", k)
		}
		seen[k] = v
	}
	if seen["PYTHONDONTWRITEBYTECODE"] != "1" || seen["PYTHONUNBUFFERED"] != "1" || seen["TZ"] != "Asia/Shanghai" {
		t.Fatalf("environment: %v", seen)
	}
}
