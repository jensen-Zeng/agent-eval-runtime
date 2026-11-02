package runner

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jensen-Zeng/agent-eval-runtime/internal/domain"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/sandbox"
)

func TestHarborAgentTimeoutExcludesVerifierAndCleanupReserve(t *testing.T) {
	task := runnerTask()
	task.Backend = "harbor"
	task.Harbor = &domain.HarborSpec{AgentImage: "sha256:" + strings.Repeat("a", 64), VerifierImage: "sha256:" + strings.Repeat("b", 64), Workdir: "/app", PassThreshold: 1}
	task.Budget.RunSeconds = 86
	task.Budget.CollectSeconds = 30
	task.Budget.EvalSeconds = 5
	task.Budget.CleanupSeconds = 50
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	start := time.Now()
	report, err := Run(ctx, &fakeFactory{}, task, agentFunc(func(c context.Context, _ sandbox.Sandbox, _ domain.Task, _ domain.Emit) (domain.AgentResult, error) {
		<-c.Done()
		return domain.AgentResult{}, domain.Fail("timeout", c.Err())
	}), Options{ArtifactsDir: t.TempDir()})
	if err == nil || report.Status != "TIMED_OUT" || time.Since(start) > 3*time.Second {
		t.Fatalf("declared 1s Agent deadline not applied: elapsed=%s report=%+v err=%v", time.Since(start), report, err)
	}
}
