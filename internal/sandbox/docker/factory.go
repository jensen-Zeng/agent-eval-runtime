package docker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/jensen-Zeng/agent-eval-runtime/internal/sandbox"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/strslice"
	"github.com/docker/docker/client"
	"github.com/opencontainers/image-spec/specs-go/v1"
)

type Options struct{ Host, Image string }

type Factory struct {
	cli   *client.Client
	image string
}

func New(ctx context.Context, opts Options) (*Factory, error) {
	var cli *client.Client
	var err error
	if strings.TrimSpace(opts.Host) != "" {
		cli, err = client.NewClientWithOpts(client.WithHost(opts.Host), client.WithAPIVersionNegotiation())
	} else if strings.TrimSpace(strings.TrimSpace(getenv("DOCKER_HOST"))) != "" {
		cli, err = client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	} else {
		// Docker CLI contexts are trusted local configuration. Inspect only resolves
		// the selected daemon endpoint; task commands never run on the host.
		host, e := contextHost(ctx)
		if e != nil {
			return nil, e
		}
		cli, err = client.NewClientWithOpts(client.WithHost(host), client.WithAPIVersionNegotiation())
	}
	if err != nil {
		return nil, fmt.Errorf("create docker client: %w", err)
	}
	return &Factory{cli: cli, image: opts.Image}, nil
}

var getenv = func(k string) string { return lookupEnv(k) }

func contextHost(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}")
	b, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("inspect Docker context endpoint: %w", err)
	}
	host := strings.TrimSpace(string(b))
	if host == "" {
		return "", fmt.Errorf("Docker context returned an empty endpoint")
	}
	return host, nil
}

func (f *Factory) Close() error {
	if f == nil || f.cli == nil {
		return nil
	}
	return f.cli.Close()
}

// DOCKER_API_VERSION is intentionally handled by the SDK's FromEnv; this tiny
// indirection also makes endpoint selection deterministic in unit tests.
func lookupEnv(k string) string { return os.Getenv(k) }

func (f *Factory) Doctor(ctx context.Context) (sandbox.Info, error) {
	info, err := f.cli.Info(ctx)
	if err != nil {
		return sandbox.Info{}, fmt.Errorf("docker info: %w", err)
	}
	ver, err := f.cli.ServerVersion(ctx)
	if err != nil {
		return sandbox.Info{}, fmt.Errorf("docker server version: %w", err)
	}
	out := sandbox.Info{ServerVersion: ver.Version, OSType: info.OSType, Architecture: info.Architecture, CPUs: info.NCPU, MemoryBytes: info.MemTotal}
	if f.image != "" {
		if im, _, e := f.cli.ImageInspectWithRaw(ctx, f.image); e == nil {
			out.ImageID = im.ID
		}
	}
	return out, nil
}

func (f *Factory) EnsureImage(ctx context.Context, projectDir, tag string) (string, error) {
	if tag == "" {
		tag = "agenteval-sandbox:v1.0"
	}
	return ensureImage(ctx, f.cli, projectDir, tag)
}

func (f *Factory) Create(ctx context.Context, spec sandbox.Spec, files sandbox.Files) (sandbox.Sandbox, error) {
	if err := validateSeed(files); err != nil {
		return nil, err
	}
	img := spec.Image
	if img == "" {
		img = f.image
	}
	if img == "" {
		img = "agenteval-sandbox:v1.0"
	}
	if _, _, err := f.cli.ImageInspectWithRaw(ctx, img); err != nil {
		return nil, fmt.Errorf("sandbox image %q unavailable: %w", img, err)
	}
	daemon, err := f.cli.Info(ctx)
	if err != nil {
		return nil, fmt.Errorf("inspect Docker resource capabilities: %w", err)
	}
	seccomp := false
	for _, opt := range daemon.SecurityOptions {
		if strings.Contains(opt, "name=seccomp") && !strings.Contains(opt, "unconfined") {
			seccomp = true
		}
	}
	if !seccomp {
		return nil, fmt.Errorf("Docker daemon must have its seccomp filter enabled")
	}
	if !daemon.MemoryLimit || !daemon.SwapLimit || !daemon.CPUCfsQuota || !daemon.PidsLimit {
		return nil, fmt.Errorf("Docker daemon lacks required resource controls (memory=%t swap=%t cpu_quota=%t pids=%t)", daemon.MemoryLimit, daemon.SwapLimit, daemon.CPUCfsQuota, daemon.PidsLimit)
	}
	cpu := spec.Limits.CPUs
	if cpu <= 0 {
		cpu = 2
	}
	mem := spec.Limits.MemoryMiB
	if mem <= 0 {
		mem = 2048
	}
	pids := spec.Limits.Pids
	if pids <= 0 {
		pids = 256
	}
	ws := spec.Limits.WorkspaceMiB
	if ws <= 0 {
		ws = 64
	}
	tmp := spec.Limits.TmpMiB
	if tmp == 0 {
		tmp = 256
	}
	if tmp < 64 || tmp > 1024 || (spec.Limits.TmpMiB != 0 && tmp > mem) {
		return nil, fmt.Errorf("invalid bounded temporary filesystem limit")
	}
	labels := map[string]string{"agenteval.project": "unknown", "agenteval.runtime": "v0.1", "agenteval.run": "unknown", "agenteval.attempt": "0"}
	for k, v := range spec.Labels {
		labels[k] = v
	}
	labels["agenteval.runtime"] = "v0.1"
	cfg := &container.Config{Image: img, User: "10001:10001", WorkingDir: "/workspace", Entrypoint: strslice.StrSlice{"/bin/sleep"}, Cmd: []string{"infinity"}, Labels: labels, Env: []string{"HOME=/tmp", "GOCACHE=/tmp/go-build", "GOMODCACHE=/tmp/go-mod", "GOPATH=/tmp/go", "GOTOOLCHAIN=local", "GOPROXY=off", "GOSUMDB=off", "GOFLAGS=-p=2", "GOMAXPROCS=2"}}
	host := &container.HostConfig{ReadonlyRootfs: true, NetworkMode: container.NetworkMode("none"), CapDrop: strslice.StrSlice{"ALL"}, SecurityOpt: []string{"no-new-privileges:true"}, Resources: container.Resources{NanoCPUs: int64(cpu * 1e9), Memory: mem * 1024 * 1024, MemorySwap: mem * 1024 * 1024, PidsLimit: &pids}, Tmpfs: map[string]string{"/tmp": fmt.Sprintf("rw,exec,nosuid,nodev,size=%dM,uid=10001,gid=10001,mode=1777", tmp)}, Mounts: []mount.Mount{{Type: mount.TypeTmpfs, Target: "/workspace", TmpfsOptions: &mount.TmpfsOptions{SizeBytes: ws * 1024 * 1024, Mode: 01777}}}}
	created, err := f.cli.ContainerCreate(ctx, cfg, host, &network.NetworkingConfig{}, (*v1.Platform)(nil), "")
	if err != nil {
		return nil, fmt.Errorf("create sandbox container: %w", err)
	}
	s := &instance{cli: f.cli, id: created.ID}
	if err = f.cli.ContainerStart(ctx, s.id, container.StartOptions{}); err != nil {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = s.Destroy(cleanup)
		cancel()
		return nil, fmt.Errorf("start sandbox: %w", err)
	}
	if err = verifySandboxConfig(ctx, f.cli, s.id, labels, cpu, mem, pids, ws, tmp); err != nil {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = s.Destroy(cleanup)
		cancel()
		return nil, err
	}
	if err = s.seed(ctx, files); err != nil {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = s.Destroy(cleanup)
		cancel()
		return nil, err
	}
	return s, nil
}

func verifySandboxConfig(ctx context.Context, cli *client.Client, id string, labels map[string]string, cpu float64, memoryMiB, pids, workspaceMiB, tmpMiB int64) error {
	in, err := cli.ContainerInspect(ctx, id)
	if err != nil {
		return fmt.Errorf("verify sandbox isolation config: %w", err)
	}
	h := in.HostConfig
	if in.Config.User != "10001:10001" || in.Config.Labels["agenteval.runtime"] != "v0.1" {
		return fmt.Errorf("sandbox identity labels or non-root user were not applied")
	}
	for _, key := range []string{"agenteval.project", "agenteval.run", "agenteval.attempt"} {
		if in.Config.Labels[key] != labels[key] {
			return fmt.Errorf("sandbox label %q was not applied", key)
		}
	}
	if !h.ReadonlyRootfs || h.NetworkMode != container.NetworkMode("none") || h.Memory != memoryMiB*1024*1024 || h.MemorySwap != memoryMiB*1024*1024 || h.NanoCPUs != int64(cpu*1e9) || h.PidsLimit == nil || *h.PidsLimit != pids {
		return fmt.Errorf("Docker did not apply requested sandbox isolation/resource limits")
	}
	if len(h.CapDrop) == 0 || h.CapDrop[0] != "ALL" {
		return fmt.Errorf("Docker did not drop all Linux capabilities")
	}
	noNewPrivs := false
	for _, opt := range h.SecurityOpt {
		if opt == "no-new-privileges:true" {
			noNewPrivs = true
		}
	}
	if !noNewPrivs {
		return fmt.Errorf("Docker did not apply no-new-privileges")
	}
	if len(h.Binds) > 0 || len(h.VolumesFrom) > 0 || len(h.Mounts) != 1 || h.Mounts[0].Type != mount.TypeTmpfs || h.Mounts[0].Target != "/workspace" {
		return fmt.Errorf("unexpected host or external mount in sandbox")
	}
	if h.Mounts[0].TmpfsOptions == nil || h.Mounts[0].TmpfsOptions.SizeBytes != workspaceMiB*1024*1024 || len(h.Tmpfs) != 1 || !hasTmpfsOption(h.Tmpfs["/tmp"], fmt.Sprintf("size=%dM", tmpMiB)) || !hasTmpfsOption(h.Tmpfs["/tmp"], "exec") || !hasTmpfsOption(h.Tmpfs["/tmp"], "nosuid") || !hasTmpfsOption(h.Tmpfs["/tmp"], "nodev") {
		return fmt.Errorf("Docker did not apply workspace/tmpfs size limits")
	}
	for _, m := range in.Mounts {
		if m.Type != "tmpfs" || (m.Destination != "/workspace" && m.Destination != "/tmp") {
			return fmt.Errorf("unexpected host or external mount in sandbox")
		}
	}
	if in.Config.Volumes != nil {
		return fmt.Errorf("unexpected image volume declaration in sandbox")
	}
	return nil
}

func hasTmpfsOption(value, option string) bool {
	for _, item := range strings.Split(value, ",") {
		if item == option {
			return true
		}
	}
	return false
}
