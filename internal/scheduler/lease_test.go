package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/jensen-Zeng/agent-eval-runtime/internal/jobs"
)

type heartbeatStore struct {
	jobs.Store
	renewed chan struct{}
}

func (s *heartbeatStore) OwnerID() string { return "0123456789abcdef01234567" }
func (s *heartbeatStore) Renew(context.Context) error {
	s.renewed <- struct{}{}
	return nil
}
func (s *heartbeatStore) Health(context.Context) error { return nil }

func TestHealthLoopRenewsLeaseDuringDrain(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &heartbeatStore{renewed: make(chan struct{}, 4)}
	service := &Service{
		store:      store,
		active:     map[string]context.CancelFunc{"job": func() {}},
		draining:   true,
		stopClaims: make(chan struct{}),
		fatal:      make(chan error, 1),
		ctx:        ctx,
		cancel:     cancel,
	}
	done := make(chan struct{})
	service.wg.Add(1)
	go func() {
		service.healthLoop()
		close(done)
	}()

	for i := 0; i < 2; i++ {
		select {
		case <-store.renewed:
		case <-time.After(3 * time.Second):
			t.Fatalf("lease renewal %d did not occur during graceful drain", i+1)
		}
		if i == 0 {
			service.mu.Lock()
			service.active["job"] = func() {}
			service.mu.Unlock()
		}
	}

	service.mu.Lock()
	delete(service.active, "job")
	service.mu.Unlock()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("health loop did not stop after drain completed")
	}

	// Once drained, healthLoop must stop renewing so shutdown can release the
	// lease instead of keeping a dead controller alive for another TTL.
	select {
	case <-store.renewed:
		t.Fatal("lease renewed after graceful drain completed")
	case <-time.After(100 * time.Millisecond):
	}
}
