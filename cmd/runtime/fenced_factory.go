package main

import (
	"github.com/jensen-Zeng/agent-eval-runtime/internal/sandbox"
	"context"
	"time"
)

type fencedFactory struct {
	sandbox.Factory
	check func(context.Context) error
}

func (f fencedFactory) Create(ctx context.Context, spec sandbox.Spec, files sandbox.Files) (sandbox.Sandbox, error) {
	check, cancel := context.WithTimeout(ctx, 3*time.Second)
	err := f.check(check)
	cancel()
	if err != nil {
		return nil, err
	}
	instance, err := f.Factory.Create(ctx, spec, files)
	if err != nil {
		return nil, err
	}
	check, cancel = context.WithTimeout(ctx, 3*time.Second)
	err = f.check(check)
	cancel()
	if err != nil {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		_ = instance.Destroy(cleanup)
		stop()
		return nil, err
	}
	return instance, nil
}
