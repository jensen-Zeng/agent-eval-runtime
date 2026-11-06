package main

import (
	"context"

	"github.com/jensen-Zeng/agent-eval-runtime/internal/jobs"
)

// Dataset versions freeze task materials independently of any model variant.
func (r *taskRegistry) datasetSnapshot(ctx context.Context, id string) (jobs.TaskSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return jobs.TaskSnapshot{}, err
	}
	t, ok := r.loaded[id]
	if !ok {
		return jobs.TaskSnapshot{}, jobs.ErrInvalidRequest
	}
	s := jobs.TaskSnapshot{TaskRevision: t.Revision, ManifestSHA256: r.hashes[id], SourceSHA256: t.SourceSHA256, EvaluatorSHA256: t.EvaluatorSHA256, ImageID: r.imageID}
	if t.IsHarbor() {
		s.Backend = "harbor"
		s.PackageSHA256 = t.Harbor.PackageSHA256
		s.VerifierImageID = t.Harbor.VerifierImage
		s.ImageID = t.Harbor.AgentImage
	}
	return s, nil
}
