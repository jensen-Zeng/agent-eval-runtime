package docker

import (
	"github.com/jensen-Zeng/agent-eval-runtime/internal/jobs"
	"context"
	"errors"
	"fmt"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
)

func (f *Factory) Reconcile(ctx context.Context, store jobs.Store, epoch int64) (int, error) {
	if err := store.Health(ctx); err != nil {
		return 0, err
	}
	owned, err := f.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: filters.NewArgs(filters.Arg("label", "agenteval.service="+store.OwnerID()), filters.Arg("label", "agenteval.runtime=v0.1"))})
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, listed := range owned {
		in, err := f.cli.ContainerInspect(ctx, listed.ID)
		if client.IsErrNotFound(err) {
			continue
		}
		if err != nil {
			return removed, err
		}
		if in.Config == nil {
			continue
		}
		labels := in.Config.Labels
		if labels["agenteval.service"] != store.OwnerID() || labels["agenteval.runtime"] != "v0.1" {
			continue
		}
		// A valid live attempt is kept. Missing/malformed job labels are orphans.
		orphan := !validOwnerID(labels["agenteval.job"]) || labels["agenteval.epoch"] != fmt.Sprint(epoch)
		if !orphan {
			job, err := store.Get(ctx, labels["agenteval.job"])
			if errors.Is(err, jobs.ErrNotFound) {
				orphan = true
			} else if err != nil {
				return removed, err
			} else {
				orphan = job.Status != jobs.StatusRunning || job.RunID != labels["agenteval.run"] || job.AttemptID != labels["agenteval.attempt"]
			}
		}
		if !orphan {
			continue
		}
		if err := store.Health(ctx); err != nil {
			return removed, err
		}
		if err := (&instance{cli: f.cli, id: in.ID}).Destroy(ctx); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}
