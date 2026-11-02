package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/jensen-Zeng/agent-eval-runtime/internal/agent"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/api"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/config"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/domain"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/jobs"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/runner"
	dockersandbox "github.com/jensen-Zeng/agent-eval-runtime/internal/sandbox/docker"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/scheduler"
	postgres "github.com/jensen-Zeng/agent-eval-runtime/internal/store"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/task"
)

// The registry is operator-owned. Requests select IDs; they cannot select paths,
// images, resource limits, Docker options, API keys or provider endpoints.
type taskRegistry struct {
	loaded     map[string]domain.Task
	hashes     map[string]string
	cacheBytes int64
	root       string
	manifests  map[string]string
	infos      []jobs.TaskInfo
	config     config.Config
	provider   *guiProvider
	imageID    string
}

func newTaskRegistry(root string, c config.Config, imageID string) (*taskRegistry, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(root, "tasks", "fixtures")
	st, err := os.Lstat(dir)
	if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("task registry must be a real directory")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	manifests := []string{}
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("task registry links are forbidden")
		}
		if entry.IsDir() {
			manifests = append(manifests, filepath.Join(dir, entry.Name(), "task.json"))
		}
	}
	harborDir := filepath.Join(root, "tasks", "harbor")
	if hi, e := os.Lstat(harborDir); e == nil {
		if !hi.IsDir() || hi.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("Harbor registry must be a real directory")
		}
		hs, e := os.ReadDir(harborDir)
		if e != nil {
			return nil, e
		}
		for _, h := range hs {
			if h.Type()&os.ModeSymlink != 0 {
				return nil, fmt.Errorf("Harbor registry links forbidden")
			}
			if h.IsDir() {
				manifests = append(manifests, filepath.Join(harborDir, h.Name(), "task.json"))
			}
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return nil, e
	}
	reg := &taskRegistry{loaded: map[string]domain.Task{}, hashes: map[string]string{}, root: root, manifests: map[string]string{}, config: c, imageID: imageID}
	for _, manifest := range manifests {
		info, err := os.Lstat(manifest)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("invalid registry manifest")
		}
		before, err := os.ReadFile(manifest)
		if err != nil || len(before) > 2<<20 {
			return nil, fmt.Errorf("registry manifest unreadable or oversized")
		}
		loaded, err := task.Load(manifest, root)
		if err != nil {
			return nil, fmt.Errorf("task %s: %w", filepath.Base(filepath.Dir(manifest)), err)
		}
		if _, exists := reg.manifests[loaded.ID]; exists {
			return nil, fmt.Errorf("duplicate task ID")
		}
		scripts := make([]string, 0, len(loaded.Scripts))
		for name := range loaded.Scripts {
			scripts = append(scripts, name)
		}
		sort.Strings(scripts)
		bytes, err := os.ReadFile(manifest)
		if err != nil {
			return nil, err
		}
		if digest(before) != digest(bytes) {
			return nil, fmt.Errorf("task changed during registry startup")
		}
		for _, data := range loaded.Source {
			reg.cacheBytes += int64(len(data))
		}
		for _, data := range loaded.Evaluator {
			reg.cacheBytes += int64(len(data))
		}
		if reg.cacheBytes > 256<<20 {
			return nil, fmt.Errorf("registered bundles exceed 256 MiB cache budget")
		}
		reg.loaded[loaded.ID] = loaded
		reg.hashes[loaded.ID] = digest(bytes)
		reg.manifests[loaded.ID] = manifest
		reg.infos = append(reg.infos, jobs.TaskInfo{Backend: loaded.Backend, ID: loaded.ID, Revision: loaded.Revision, Prompt: loaded.Prompt, Scripts: scripts})
	}
	if len(reg.infos) == 0 {
		return nil, fmt.Errorf("no registered tasks")
	}
	sort.Slice(reg.infos, func(i, j int) bool { return reg.infos[i].ID < reg.infos[j].ID })
	return reg, nil
}

func digest(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }

func (r *taskRegistry) selectTask(req jobs.Request) (jobs.Request, domain.Task, domain.Agent, any, jobs.TaskSnapshot, error) {
	var empty jobs.TaskSnapshot
	if req.SchemaVersion == 0 {
		req.SchemaVersion = 1
	}
	if req.Agent == "" {
		req.Agent = "scripted"
	}
	if req.Agent == "scripted" && req.Script == "" {
		req.Script = "success"
	}
	if req.SchemaVersion != 1 || len(req.TaskID) > 80 || len(req.Script) > 80 || len(req.Model) > 256 {
		return req, domain.Task{}, nil, nil, empty, jobs.ErrInvalidRequest
	}
	manifest, found := r.manifests[req.TaskID]
	if !found {
		return req, domain.Task{}, nil, nil, empty, jobs.ErrInvalidRequest
	}
	_ = manifest
	loaded := r.loaded[req.TaskID]
	var err error
	var selected domain.Agent
	var public any
	switch req.Agent {
	case "scripted":
		if req.Model != "" {
			return req, loaded, nil, nil, empty, jobs.ErrInvalidRequest
		}
		script, ok := loaded.Scripts[req.Script]
		if !ok {
			return req, loaded, nil, nil, empty, jobs.ErrInvalidRequest
		}
		selected = agent.Scripted{Script: script}
		public = map[string]any{"kind": "scripted", "name": req.Script, "actions": script.Actions}
	case "llm":
		if req.Script != "" {
			return req, loaded, nil, nil, empty, jobs.ErrInvalidRequest
		}
		pc := guiProviderConfig{r.config.LLM.BaseURL, r.config.LLM.Model, r.config.LLM.MaxRetries}
		key := os.Getenv(r.config.LLM.APIKeyEnv)
		if r.provider != nil {
			pc, key = r.provider.snapshot()
		}
		if req.Model == "" {
			req.Model = pc.Model
		}
		if req.Model == "" || len(req.Model) > 256 {
			return req, loaded, nil, nil, empty, jobs.ErrInvalidRequest
		}
		if key == "" {
			return req, loaded, nil, nil, empty, jobs.ErrUnavailable
		}
		selected = agent.LLM{Config: agent.Config{BaseURL: pc.BaseURL, Model: req.Model, APIKey: key, MaxRetries: pc.MaxRetries}}
		public = map[string]any{"kind": "openai-chat-completions", "base_url": pc.BaseURL, "model": req.Model, "max_retries": pc.MaxRetries}

	default:
		return req, loaded, nil, nil, empty, jobs.ErrInvalidRequest
	}
	encoded, err := json.Marshal(public)
	if err != nil {
		return req, loaded, nil, nil, empty, jobs.ErrUnavailable
	}
	snapshot := jobs.TaskSnapshot{TaskRevision: loaded.Revision, ManifestSHA256: r.hashes[req.TaskID], SourceSHA256: loaded.SourceSHA256, EvaluatorSHA256: loaded.EvaluatorSHA256, ImageID: r.imageID, AgentConfigSHA256: digest(encoded)}
	if loaded.IsHarbor() {
		snapshot.Backend = "harbor"
		snapshot.PackageSHA256 = loaded.Harbor.PackageSHA256
		snapshot.VerifierImageID = loaded.Harbor.VerifierImage
		snapshot.ImageID = loaded.Harbor.AgentImage
	}
	return req, loaded, selected, public, snapshot, nil
}

func (r *taskRegistry) prepare(ctx context.Context, req jobs.Request) (jobs.Request, jobs.TaskSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return req, jobs.TaskSnapshot{}, err
	}
	normalized, _, _, _, snapshot, err := r.selectTask(req)
	return normalized, snapshot, err
}

func serveRuntime(parent context.Context, c config.Config, project string, out, errout io.Writer) int {
	if err := c.Server.Validate(); err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	root, err := filepath.Abs(project)
	if err != nil {
		fmt.Fprintln(errout, "invalid project root")
		return 1
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		fmt.Fprintln(errout, "project root unavailable")
		return 1
	}
	if !filepath.IsAbs(c.ArtifactsDir) {
		c.ArtifactsDir = filepath.Join(root, c.ArtifactsDir)
	}
	dsn := os.Getenv(c.Server.DatabaseURLEnv)
	if dsn == "" {
		fmt.Fprintf(errout, "set PostgreSQL connection URL in %s\n", c.Server.DatabaseURLEnv)
		return 1
	}
	initCtx, initCancel := context.WithTimeout(parent, 45*time.Second)
	store, err := postgres.New(initCtx, dsn, c.Server.Workers+4)
	initCancel()
	if err != nil {
		fmt.Fprintln(errout, "database startup:", err)
		return 1
	}
	// Keep lease live during potentially slow registry/cleanup startup. Scheduler later owns renewal.
	startupCtx, startupCancel := context.WithCancel(parent)
	startupDone := make(chan struct{})
	go func() {
		defer close(startupDone)
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-startupCtx.Done():
				return
			case <-ticker.C:
				check, cancel := context.WithTimeout(startupCtx, 3*time.Second)
				err := store.Renew(check)
				cancel()
				if err != nil {
					return
				}
			}
		}
	}()
	defer func() { startupCancel(); <-startupDone }()
	// Resources are only closed after the scheduler has finished its workers.
	closeResources := true
	defer func() {
		if closeResources {
			startupCancel()
			<-startupDone
			_ = store.Close()
		}
	}()
	connectCtx, connectCancel := context.WithTimeout(parent, 15*time.Second)
	factory, err := dockersandbox.New(connectCtx, dockersandbox.Options{Host: c.DockerHost, Image: c.Image})
	connectCancel()
	if err != nil {
		fmt.Fprintln(errout, "Docker connection unavailable")
		return 1
	}
	defer func() {
		if closeResources {
			_ = factory.Close()
		}
	}()
	doctorCtx, doctorCancel := context.WithTimeout(parent, 15*time.Second)
	info, err := factory.Doctor(doctorCtx)
	doctorCancel()
	if err != nil || info.OSType != "linux" || (info.Architecture != "amd64" && info.Architecture != "x86_64") || info.ImageID == "" {
		fmt.Fprintln(errout, "Linux amd64 Docker engine and prepared sandbox image required; run runtime prepare first")
		return 1
	}
	registry, err := newTaskRegistry(root, c, info.ImageID)
	if err != nil {
		fmt.Fprintln(errout, "task registry:", err)
		return 1
	}
	provider, err := newGUIProvider(root, c)
	if err != nil {
		fmt.Fprintln(errout, "saved UI provider settings or key vault are unavailable")
		return 1
	}
	registry.provider = provider
	maxMemory := int64(0)
	for _, t := range registry.loaded {
		if t.Limits.MemoryMiB > maxMemory {
			maxMemory = t.Limits.MemoryMiB
		}
	}
	if info.MemoryBytes > 0 && int64(c.Server.Workers)*maxMemory*(1<<20) > info.MemoryBytes*3/4 {
		fmt.Fprintln(errout, "worker memory reservation exceeds 75% of Docker engine RAM; reduce workers or task limits")
		return 1
	}
	// Bind before recovery: a port conflict must not change job state or containers.
	listener, err := net.Listen("tcp", c.Server.Listen)
	if err != nil {
		fmt.Fprintln(errout, "cannot bind local API listener")
		return 1
	}
	defer listener.Close()
	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok || !addr.IP.IsLoopback() {
		fmt.Fprintln(errout, "API listener must be loopback")
		return 1
	}
	recoverCtx, recoverCancel := context.WithTimeout(parent, 60*time.Second)
	cleaned, err := factory.CleanupOwnedGuarded(recoverCtx, store.OwnerID(), store.Health)
	if err == nil {
		_, err = store.Recover(recoverCtx)
	}
	recoverCancel()
	if err != nil {
		fmt.Fprintln(errout, "startup resource cleanup or queue recovery failed")
		return 1
	}
	run := func(ctx context.Context, job jobs.Job, started func(string, string) error) (runner.Report, error) {
		failure := func(reason string, cause error) (runner.Report, error) {
			return runner.Report{SchemaVersion: 1, Version: runner.Version, TaskID: job.Request.TaskID, TaskRevision: job.TaskSnapshot.TaskRevision, Status: jobs.StatusInfraError, Verdict: "infra_error", Reason: reason}, cause
		}
		if err := ctx.Err(); err != nil {
			return runner.Report{SchemaVersion: 1, Version: runner.Version, TaskID: job.Request.TaskID, Status: jobs.StatusCanceled, Verdict: "canceled", Reason: "canceled"}, err
		}
		checkCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		leaseErr := store.Health(checkCtx)
		cancel()
		if leaseErr != nil {
			return failure("lease_lost", leaseErr)
		}
		manifest := registry.manifests[job.Request.TaskID]
		fresh, loadErr := task.Load(manifest, root)
		if loadErr != nil {
			return failure("snapshot_changed", loadErr)
		}
		manifestBytes, readErr := os.ReadFile(manifest)
		if readErr != nil || digest(manifestBytes) != job.TaskSnapshot.ManifestSHA256 || fresh.SourceSHA256 != job.TaskSnapshot.SourceSHA256 || fresh.EvaluatorSHA256 != job.TaskSnapshot.EvaluatorSHA256 {
			return failure("snapshot_changed", jobs.ErrConflict)
		}
		if job.Request.Agent == "llm" {
			if err := provider.waitForKey(ctx); err != nil {
				return runner.Report{Status: jobs.StatusCanceled, Verdict: "canceled", Reason: "canceled"}, err
			}
		}
		normalized, loaded, selected, public, snapshot, err := registry.selectTask(job.Request)
		if err != nil {
			return failure("task_or_agent_unavailable", err)
		}
		if normalized != job.Request || snapshot != job.TaskSnapshot {
			return failure("snapshot_changed", jobs.ErrConflict)
		}
		return runner.Run(ctx, fencedFactory{Factory: factory, check: store.Health}, loaded, selected, runner.Options{Image: job.TaskSnapshot.ImageID, ArtifactsDir: c.ArtifactsDir, AgentName: job.Request.Agent, AgentConfig: public, OnStarted: started, Labels: map[string]string{"agenteval.service": store.OwnerID(), "agenteval.job": job.ID, "agenteval.epoch": fmt.Sprint(store.Epoch())}})
	}
	var readyMu sync.Mutex
	var readyAt time.Time
	var readyErr error
	ready := func(ctx context.Context) error {
		readyMu.Lock()
		defer readyMu.Unlock()
		if time.Since(readyAt) < time.Second {
			return readyErr
		}
		readyAt = time.Now()
		checkCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		info, err := factory.Doctor(checkCtx)
		if err != nil || info.ImageID != registry.imageID {
			readyErr = jobs.ErrUnavailable
			return jobs.ErrUnavailable
		}
		readyErr = nil
		return nil
	}
	service, err := scheduler.New(scheduler.Options{Store: store, Workers: c.Server.Workers, QueueCapacity: c.Server.QueueCapacity, PollInterval: time.Duration(c.Server.PollIntervalMS) * time.Millisecond, Prepare: registry.prepare, Execute: run, Tasks: registry.infos, ArtifactsDir: c.ArtifactsDir, ReadyCheck: ready})
	if err != nil {
		fmt.Fprintln(errout, "cannot configure scheduler")
		return 1
	}
	dashboard, err := newGUIDashboard(parent, root, c, service, registry, provider, "http://"+listener.Addr().String())
	if err != nil {
		fmt.Fprintln(errout, "cannot initialize local dashboard storage")
		return 1
	}
	defer dashboard.manager.Close()
	server := &http.Server{Handler: api.NewWithUI(guiGuardBackend{Service: service, provider: provider}, dashboard), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	serveErrors := make(chan error, 1)
	startupCancel()
	<-startupDone
	service.Start()
	reconcileCtx, reconcileCancel := context.WithCancel(context.Background())
	reconcileDone := make(chan struct{})
	defer func() { reconcileCancel(); <-reconcileDone }()
	go func() {
		defer close(reconcileDone)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-reconcileCtx.Done():
				return
			case <-ticker.C:
				check, cancel := context.WithTimeout(reconcileCtx, 4*time.Second)
				removed, reconcileErr := factory.Reconcile(check, store, store.Epoch())
				service.RecordReconcile(removed, reconcileErr)
				cancel()
			}
		}
	}()
	go func() { serveErrors <- server.Serve(listener) }()
	fmt.Fprintf(out, "AgentEval Runtime v%s listening on http://%s; workers=%d queue_capacity=%d recovered_containers=%d\n", runner.Version, listener.Addr(), c.Server.Workers, c.Server.QueueCapacity, cleaned)
	exit := 0
	select {
	case <-parent.Done():
	case <-service.Fatal():
		fmt.Fprintln(errout, "scheduler stopped after database or persistence failure")
		exit = 1
	case err := <-serveErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(errout, "HTTP server stopped unexpectedly")
			exit = 1
		}
	}
	dashboard.manager.Close()
	// Stop claims immediately while shutting down HTTP and draining existing work.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), time.Duration(c.Server.ShutdownSeconds)*time.Second)
	defer shutdownCancel()
	httpDone := make(chan struct{})
	go func() {
		if server.Shutdown(shutdownCtx) != nil {
			_ = server.Close()
		}
		close(httpDone)
	}()
	if err := service.Stop(shutdownCtx); err != nil {
		fmt.Fprintln(errout, "graceful drain deadline exceeded; active jobs canceled")
		exit = 1
	}
	<-httpDone
	select {
	case <-service.Done():
	default:
		// Leave connections open until process exit; do not race a stuck worker.
		// On the next start, owner cleanup and interrupted-job recovery run first.
		closeResources = false
		fmt.Fprintln(errout, "workers did not fully stop; resources will be recovered on next startup")
		exit = 1
	}
	return exit
}
