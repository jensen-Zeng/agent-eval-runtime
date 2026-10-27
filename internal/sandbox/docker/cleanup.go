package docker

import (
	"context"
	"errors"
	"fmt"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
)

// CleanupOwned stops and removes only v0.1 runtime containers owned by ownerID.
// It never performs a daemon-wide prune or touches unlabelled resources.
func (f *Factory) CleanupOwned(ctx context.Context, ownerID string) (int, error) {
	return f.CleanupOwnedGuarded(ctx, ownerID, nil)
}

func (f *Factory) CleanupOwnedGuarded(ctx context.Context, ownerID string, check func(context.Context) error) (int, error) {
	if check != nil {
		if err := check(ctx); err != nil {
			return 0, err
		}
	}
	if !validOwnerID(ownerID) {
		return 0, fmt.Errorf("owner id must be 24 lowercase hexadecimal characters")
	}
	args := filters.NewArgs(
		filters.Arg("label", "agenteval.service="+ownerID),
		filters.Arg("label", "agenteval.runtime=v0.1"),
	)
	containers, err := f.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: args})
	if err != nil {
		return 0, fmt.Errorf("list owned sandbox containers: %w", err)
	}
	cleaned := 0
	var failures []error
	for _, listed := range containers {
		if err := ctx.Err(); err != nil {
			failures = append(failures, err)
			break
		}
		in, err := f.cli.ContainerInspect(ctx, listed.ID)
		if err != nil {
			if client.IsErrNotFound(err) {
				continue
			}
			failures = append(failures, fmt.Errorf("inspect candidate container %s: %w", shortID(listed.ID), err))
			continue
		}
		labels := in.Config.Labels
		if labels["agenteval.service"] != ownerID || labels["agenteval.runtime"] != "v0.1" {
			continue
		}
		if check != nil {
			if err := check(ctx); err != nil {
				return cleaned, err
			}
		}
		s := &instance{cli: f.cli, id: in.ID}
		if err = s.Destroy(ctx); err != nil {
			failures = append(failures, fmt.Errorf("remove owned container %s: %w", shortID(in.ID), err))
			continue
		}
		cleaned++
	}
	return cleaned, errors.Join(failures...)
}

func validOwnerID(s string) bool {
	if len(s) != 24 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
