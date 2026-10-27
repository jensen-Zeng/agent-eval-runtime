package task

import (
	"github.com/jensen-Zeng/agent-eval-runtime/internal/domain"
	"strings"
	"testing"
)

func TestCommandEnvironmentIsOfflineAndImageOwned(t *testing.T) {
	task := domain.Task{Environment: domain.ExecutionEnvironment{GoModuleCache: "/opt/agenteval/go-mod"}}
	got := strings.Join(task.CommandEnvironment(), "\n")
	for _, v := range []string{"GOTOOLCHAIN=local", "GOPROXY=off", "GOSUMDB=off", "GOMODCACHE=/opt/agenteval/go-mod"} {
		if !strings.Contains(got, v) {
			t.Fatal(got)
		}
	}
}
