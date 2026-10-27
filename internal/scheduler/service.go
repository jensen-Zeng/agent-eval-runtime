// Package scheduler runs durable jobs with a fixed worker pool.
package scheduler

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jensen-Zeng/agent-eval-runtime/internal/jobs"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/runner"
)

const (
	storeTimeout    = 3 * time.Second
	finishTimeout   = 10 * time.Second
	postCancelDrain = 70 * time.Second
)

var idPattern = regexp.MustCompile(`^[0-9a-f]{24}$`)

type PrepareFunc func(context.Context, jobs.Request) (jobs.Request, jobs.TaskSnapshot, error)
type ExecuteFunc func(context.Context, jobs.Job, func(runID, attemptID string) error) (runner.Report, error)
type Options struct {
	Store         jobs.Store
	Workers       int
	QueueCapacity int
	PollInterval  time.Duration
	Prepare       PrepareFunc
	Execute       ExecuteFunc
	Tasks         []jobs.TaskInfo
	ArtifactsDir  string
	ReadyCheck    func(context.Context) error
}

type Service struct {
	store        jobs.Store
	workers      int
	capacity     int
	pollInterval time.Duration
	prepare      PrepareFunc
	execute      ExecuteFunc
	tasks        []jobs.TaskInfo
	artifactsDir string
	readyCheck   func(context.Context) error
	ownerToken   string

	heartbeatUnix        int64
	reconciledContainers int64
	reconcileErrors      int64
	mu                   sync.Mutex
	draining             bool
	started              bool
	active               map[string]context.CancelFunc
	fatalErr             error
	stopClaims           chan struct{}
	notify               chan struct{}
	fatal                chan error
	fatalOnce            sync.Once
	startOnce            sync.Once
	stopOnce             sync.Once
	doneOnce             sync.Once
	ctx                  context.Context
	cancel               context.CancelFunc
	wg                   sync.WaitGroup
	done                 chan struct{}
}

func New(opts Options) (*Service, error) {
	if opts.Store == nil {
		return nil, fmt.Errorf("scheduler store is required")
	}
	if opts.Workers < 1 || opts.QueueCapacity < 1 {
		return nil, fmt.Errorf("workers and queue capacity must be positive")
	}
	if opts.Prepare == nil || opts.Execute == nil {
		return nil, fmt.Errorf("prepare and execute functions are required")
	}
	if opts.PollInterval < 0 {
		return nil, fmt.Errorf("poll interval cannot be negative")
	}
	if opts.PollInterval == 0 {
		opts.PollInterval = time.Second
	}
	ownerID := opts.Store.OwnerID()
	if !idPattern.MatchString(ownerID) {
		return nil, fmt.Errorf("store owner id must be 24 lowercase hexadecimal characters")
	}
	if opts.ArtifactsDir == "" {
		return nil, fmt.Errorf("artifacts directory is required")
	}
	artifactsDir, err := filepath.Abs(opts.ArtifactsDir)
	if err != nil {
		return nil, fmt.Errorf("resolve artifacts directory: %w", err)
	}
	tokenBytes := make([]byte, 12)
	if _, err := rand.Read(tokenBytes); err != nil {
		return nil, fmt.Errorf("create service owner token: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	tasks := cloneTasks(opts.Tasks)
	return &Service{store: opts.Store, workers: opts.Workers, capacity: opts.QueueCapacity, pollInterval: opts.PollInterval, prepare: opts.Prepare, execute: opts.Execute, tasks: tasks, artifactsDir: artifactsDir, readyCheck: opts.ReadyCheck, ownerToken: hex.EncodeToString(tokenBytes), active: make(map[string]context.CancelFunc), stopClaims: make(chan struct{}), notify: make(chan struct{}, 1), fatal: make(chan error, 1), ctx: ctx, cancel: cancel, done: make(chan struct{})}, nil
}

func cloneTasks(in []jobs.TaskInfo) []jobs.TaskInfo {
	out := make([]jobs.TaskInfo, len(in))
	for i, t := range in {
		out[i] = t
		out[i].Scripts = append([]string(nil), t.Scripts...)
	}
	return out
}

func (s *Service) Start() {
	s.startOnce.Do(func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.draining {
			s.doneOnce.Do(func() { close(s.done) })
			return
		}
		s.started = true
		s.wg.Add(s.workers + 1)
		go func() {
			s.wg.Wait()
			s.doneOnce.Do(func() { close(s.done) })
		}()
		for i := 0; i < s.workers; i++ {
			go s.worker()
		}
		go s.healthLoop()
	})
}

func (s *Service) Fatal() <-chan error { return s.fatal }

// Done closes after every worker and the health loop have exited. Callers must
// wait for Done before closing the backing Store after a Stop timeout.
func (s *Service) Done() <-chan struct{} { return s.done }

func (s *Service) Submit(ctx context.Context, req jobs.Request, idempotencyKey string) (jobs.Job, bool, error) {
	if err := s.Ready(ctx); err != nil {
		return jobs.Job{}, false, err
	}
	expected := req.ExpectedSnapshot
	req.ExpectedSnapshot = nil
	normalized, snapshot, err := s.prepare(ctx, req)
	if err != nil {
		return jobs.Job{}, false, fmt.Errorf("prepare job snapshot: %w", err)
	}
	normalized.ExpectedSnapshot = nil
	if expected != nil && *expected != snapshot {
		return jobs.Job{}, false, jobs.ErrConflict
	}
	if err = s.accepting(); err != nil {
		return jobs.Job{}, false, err
	}
	canonical, err := json.Marshal(struct {
		Request  jobs.Request      `json:"request"`
		Snapshot jobs.TaskSnapshot `json:"task_snapshot"`
	}{normalized, snapshot})
	if err != nil {
		return jobs.Job{}, false, fmt.Errorf("canonicalize job request: %w", err)
	}
	hash := sha256.Sum256(canonical)
	s.mu.Lock()
	if s.fatalErr != nil {
		fatalErr := s.fatalErr
		s.mu.Unlock()
		return jobs.Job{}, false, fmt.Errorf("%w: %v", jobs.ErrUnavailable, fatalErr)
	}
	if s.draining {
		s.mu.Unlock()
		return jobs.Job{}, false, jobs.ErrStopping
	}
	storeCtx, storeCancel := context.WithTimeout(ctx, storeTimeout)
	job, reused, err := s.store.Submit(storeCtx, normalized, snapshot, idempotencyKey, hex.EncodeToString(hash[:]), s.capacity)
	storeCancel()
	s.mu.Unlock()
	if err != nil {
		return jobs.Job{}, false, err
	}
	if !reused {
		select {
		case s.notify <- struct{}{}:
		default:
		}
	}
	return job, reused, nil
}

func (s *Service) accepting() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fatalErr != nil {
		return fmt.Errorf("%w: %v", jobs.ErrUnavailable, s.fatalErr)
	}
	if s.draining {
		return jobs.ErrStopping
	}
	return nil
}

func (s *Service) Get(ctx context.Context, id string) (jobs.Job, error) { return s.store.Get(ctx, id) }
func (s *Service) List(ctx context.Context, limit int, status string) ([]jobs.Job, error) {
	return s.store.List(ctx, limit, status)
}

func (s *Service) Cancel(ctx context.Context, id string) (jobs.Job, error) {
	job, err := s.store.Cancel(ctx, id)
	if err != nil {
		return jobs.Job{}, err
	}
	if job.Status == jobs.StatusRunning || job.CancelRequested {
		s.cancelActive(job.ID)
	}
	return job, nil
}

func (s *Service) Tasks() []jobs.TaskInfo { return cloneTasks(s.tasks) }

func (s *Service) Health(ctx context.Context) error {
	s.mu.Lock()
	fatalErr := s.fatalErr
	s.mu.Unlock()
	if fatalErr != nil {
		return fmt.Errorf("%w: %v", jobs.ErrUnavailable, fatalErr)
	}
	return s.store.Health(ctx)
}

func (s *Service) Ready(ctx context.Context) error {
	if err := s.accepting(); err != nil {
		return err
	}
	if s.readyCheck != nil {
		if err := s.readyCheck(ctx); err != nil {
			return fmt.Errorf("readiness check: %w", err)
		}
	}
	return s.Health(ctx)
}

func (s *Service) Summary(ctx context.Context) (jobs.Summary, error) {
	result, err := s.store.Summary(ctx)
	if err != nil {
		return jobs.Summary{}, err
	}
	s.mu.Lock()
	if epoch, ok := s.store.(interface{ Epoch() int64 }); ok {
		result.ControllerEpoch = epoch.Epoch()
	}
	result.HeartbeatUnix = s.heartbeatUnix
	result.ReconciledContainers = s.reconciledContainers
	result.ReconcileErrors = s.reconcileErrors
	result.WorkerLimit = s.workers
	result.ActiveWorkers = len(s.active)
	result.Draining = s.draining
	s.mu.Unlock()
	return result, nil
}

func (s *Service) worker() {
	defer s.wg.Done()
	for {
		s.mu.Lock()
		if s.draining || s.fatalErr != nil {
			s.mu.Unlock()
			return
		}
		claimCtx, cancel := context.WithTimeout(s.ctx, storeTimeout)
		job, err := s.store.Claim(claimCtx, s.ownerToken)
		cancel()
		if err == nil {
			jobCtx, jobCancel := context.WithCancel(s.ctx)
			s.active[job.ID] = jobCancel
			s.mu.Unlock()
			s.runJob(job, jobCtx)
			jobCancel()
			s.mu.Lock()
			delete(s.active, job.ID)
			draining := s.draining || s.fatalErr != nil
			s.mu.Unlock()
			if draining {
				return
			}
			continue
		}
		s.mu.Unlock()
		if errors.Is(err, jobs.ErrNoJob) {
			timer := time.NewTimer(s.pollInterval)
			select {
			case <-s.notify:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
			case <-timer.C:
			case <-s.stopClaims:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				return
			case <-s.ctx.Done():
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				return
			}
			continue
		}
		if s.ctx.Err() != nil {
			return
		}
		s.signalFatal(fmt.Errorf("claim job: %w", err))
		return
	}
}

func (s *Service) runJob(job jobs.Job, ctx context.Context) {
	defer func() {
		if recovered := recover(); recovered != nil {
			s.finish(job, runner.Report{Status: jobs.StatusInfraError, Reason: "scheduler_panic"}, fmt.Errorf("scheduler panic: %v", recovered))
		}
	}()
	readCtx, readCancel := context.WithTimeout(context.Background(), storeTimeout)
	latest, err := s.store.Get(readCtx, job.ID)
	readCancel()
	if err != nil {
		if ctx.Err() != nil {
			s.finish(job, runner.Report{Status: jobs.StatusCanceled, Reason: "canceled"}, nil)
			return
		}
		s.signalFatal(fmt.Errorf("reload claimed job %s: %w", job.ID, err))
		return
	}
	if latest.Status != jobs.StatusRunning {
		return
	}
	if ctx.Err() != nil || latest.CancelRequested {
		s.finish(job, runner.Report{Status: jobs.StatusCanceled, Reason: "canceled"}, nil)
		return
	}
	onStarted := func(runID, attemptID string) error {
		if !idPattern.MatchString(runID) || attemptID != runID+"-1" {
			return fmt.Errorf("runner returned invalid run/attempt identity")
		}
		persistCtx, cancel := context.WithTimeout(context.Background(), storeTimeout)
		defer cancel()
		ok, e := s.store.SetAttempt(persistCtx, job.ID, s.ownerToken, runID, attemptID)
		if e != nil {
			s.signalFatal(fmt.Errorf("persist attempt metadata: %w", e))
			return fmt.Errorf("persist attempt metadata: %w", e)
		}
		if !ok {
			checkCtx, checkCancel := context.WithTimeout(context.Background(), storeTimeout)
			latest, checkErr := s.store.Get(checkCtx, job.ID)
			checkCancel()
			if checkErr == nil && (latest.CancelRequested || latest.Status == jobs.StatusCanceled) {
				return context.Canceled
			}
			s.signalFatal(jobs.ErrLeadershipLost)
			return jobs.ErrLeadershipLost
		}
		return nil
	}
	report, execErr := s.execute(ctx, job, onStarted)
	if ctx.Err() != nil && report.CleanupStatus != "failed" {
		s.mu.Lock()
		fatal := s.fatalErr != nil
		s.mu.Unlock()
		if fatal && report.Status != jobs.StatusInfraError {
			report.Status = jobs.StatusInfraError
			report.Verdict = "infra_error"
			report.Reason = "scheduler_unavailable"
		} else if !fatal && report.Status != jobs.StatusInfraError {
			report.Status = jobs.StatusCanceled
			report.Verdict = "canceled"
			report.Reason = "canceled"
			execErr = nil
		}
	}
	if report.Status == "" {
		reason := report.Reason
		if reason == "" {
			reason = "execute_error"
		}
		report.Status = jobs.StatusInfraError
		report.Reason = reason
	}
	if execErr != nil && report.Reason == "" {
		report.Reason = redact(execErr.Error())
	}
	s.finish(job, report, execErr)
}

func (s *Service) finish(job jobs.Job, report runner.Report, execErr error) {
	status := normalizeStatus(report.Status)
	if report.Status == "" {
		status = jobs.StatusInfraError
	}
	if execErr != nil && status == jobs.StatusSucceeded {
		status = jobs.StatusInfraError
		report.Status = status
		if report.Reason == "" {
			report.Reason = "execute_error"
		}
	}
	if report.CleanupStatus == "failed" {
		status = jobs.StatusInfraError
		report.Status = status
		if report.Reason == "" || report.Reason == "success" {
			report.Reason = "cleanup_error"
		}
	}
	if status == jobs.StatusInfraError && report.Reason == "" {
		report.Reason = "execute_error"
	}
	if execErr != nil && report.Reason == "" {
		report.Reason = redact(execErr.Error())
	}
	if report.Reason != "" {
		report.Reason = redact(report.Reason)
	}
	report.SchemaVersion = 1
	if report.Version == "" {
		report.Version = runner.Version
	}
	if report.TaskID == "" {
		report.TaskID = job.Request.TaskID
	}
	if report.TaskRevision == 0 {
		report.TaskRevision = job.TaskSnapshot.TaskRevision
	}
	report.Status = status
	if report.Verdict == "" || (status == jobs.StatusInfraError && report.Verdict == "success") {
		switch status {
		case jobs.StatusSucceeded:
			report.Verdict = "success"
		case jobs.StatusFailed:
			report.Verdict = "test_failure"
		case jobs.StatusTimedOut:
			report.Verdict = "timeout"
		case jobs.StatusCanceled:
			report.Verdict = "canceled"
		default:
			report.Verdict = "infra_error"
		}
	}
	result, err := json.Marshal(report)
	if err != nil {
		status = jobs.StatusInfraError
		report = runner.Report{Status: status, Reason: "result_encode_error"}
		result, _ = json.Marshal(report)
	}
	result = redactResultJSON(result)
	finishCtx, cancel := context.WithTimeout(context.Background(), finishTimeout)
	defer cancel()
	ok, err := s.store.Finish(finishCtx, job.ID, s.ownerToken, status, result, report.Reason)
	if err != nil {
		s.signalFatal(fmt.Errorf("finish job %s: %w", job.ID, err))
		return
	}
	if !ok {
		s.signalFatal(jobs.ErrLeadershipLost)
	}
}

func normalizeStatus(status string) string {
	switch strings.ToUpper(status) {
	case jobs.StatusSucceeded:
		return jobs.StatusSucceeded
	case jobs.StatusFailed:
		return jobs.StatusFailed
	case jobs.StatusTimedOut:
		return jobs.StatusTimedOut
	case jobs.StatusCanceled:
		return jobs.StatusCanceled
	case jobs.StatusInfraError:
		return jobs.StatusInfraError
	default:
		return jobs.StatusInfraError
	}
}

var secretPattern = regexp.MustCompile(`(?i)(sk-[a-z0-9_-]{16,}|bearer\s+[a-z0-9._~+/-]+=*|OPENAI_API_KEY\s*[=:]\s*[^\s,;]+)`)

func redact(s string) string { return secretPattern.ReplaceAllString(s, "[REDACTED]") }

func (s *Service) cancelActive(id string) {
	s.mu.Lock()
	cancel := s.active[id]
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *Service) signalFatal(err error) {
	if err == nil {
		return
	}
	err = errors.New(redact(err.Error()))
	s.fatalOnce.Do(func() {
		s.mu.Lock()
		s.fatalErr = err
		s.draining = true
		select {
		case <-s.stopClaims:
		default:
			close(s.stopClaims)
		}
		cancels := make([]context.CancelFunc, 0, len(s.active))
		for _, cancel := range s.active {
			cancels = append(cancels, cancel)
		}
		s.mu.Unlock()
		for _, cancel := range cancels {
			cancel()
		}
		s.cancel()
		select {
		case s.fatal <- err:
		default:
		}
	})
}

func (s *Service) healthLoop() {
	defer s.wg.Done()
	if err := s.checkStoreHealth(); err != nil {
		s.signalFatal(err)
		return
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.mu.Lock()
			drained := s.draining && len(s.active) == 0
			s.mu.Unlock()
			if drained {
				return
			}
			if err := s.checkStoreHealth(); err != nil {
				s.signalFatal(err)
				return
			}
		case <-s.ctx.Done():
			return
		}
	}
}

func (s *Service) checkStoreHealth() error {
	ctx, cancel := context.WithTimeout(s.ctx, storeTimeout)
	defer cancel()
	if renewer, ok := s.store.(interface{ Renew(context.Context) error }); ok {
		if err := renewer.Renew(ctx); err != nil {
			return fmt.Errorf("scheduler lease renewal: %w", err)
		}
	}
	if err := s.store.Health(ctx); err != nil {
		return fmt.Errorf("scheduler store health: %w", err)
	}
	s.mu.Lock()
	s.heartbeatUnix = time.Now().Unix()
	s.mu.Unlock()
	return nil
}

func (s *Service) Stop(ctx context.Context) error {
	s.stopOnce.Do(func() {
		s.mu.Lock()
		s.draining = true
		select {
		case <-s.stopClaims:
		default:
			close(s.stopClaims)
		}
		if !s.started {
			s.doneOnce.Do(func() { close(s.done) })
		}
		s.mu.Unlock()
	})
	select {
	case <-s.done:
		s.cancel()
		return nil
	case <-ctx.Done():
		s.cancelActiveAll()
		select {
		case <-s.done:
			s.cancel()
			return ctx.Err()
		case <-time.After(postCancelDrain):
			s.cancel()
			return fmt.Errorf("stop timed out after canceling active jobs: %w", ctx.Err())
		}
	}
}

func (s *Service) cancelActiveAll() {
	s.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(s.active))
	for _, cancel := range s.active {
		cancels = append(cancels, cancel)
	}
	s.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}
