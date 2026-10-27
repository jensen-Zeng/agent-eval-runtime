package scheduler

import (
	"github.com/jensen-Zeng/agent-eval-runtime/internal/jobs"
	"context"
)

// Preview freezes operator-owned inputs without reserving queue capacity.
func (s *Service) Preview(ctx context.Context, req jobs.Request) (jobs.Preview, error) {
	if err := s.Ready(ctx); err != nil {
		return jobs.Preview{}, err
	}
	req.ExpectedSnapshot = nil
	normalized, snapshot, err := s.prepare(ctx, req)
	normalized.ExpectedSnapshot = nil
	return jobs.Preview{Request: normalized, TaskSnapshot: snapshot}, err
}
