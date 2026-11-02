package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	imagetypes "github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/sandbox"
)

func TestDockerHarborWorkspaceIntegration(t *testing.T) {
	if os.Getenv("AGENTEVAL_DOCKER_TESTS") != "1" {
		t.Skip("set AGENTEVAL_DOCKER_TESTS=1 to run Docker Harbor integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	factory, err := New(ctx, Options{Image: "agenteval-sandbox:v1.0"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = factory.Close() })
	projectRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	baseID, err := factory.EnsureImage(ctx, projectRoot, "agenteval-sandbox:v1.0")
	if err != nil {
		t.Fatal(err)
	}
	agentImage, verifierImage, agentTag, verifierTag, err := buildHarborTestImages(ctx, factory, baseID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		for _, tag := range []string{verifierTag, agentTag} {
			if _, err := factory.cli.ImageRemove(cleanup, tag, imagetypes.RemoveOptions{Force: true, PruneChildren: true}); err != nil && !client.IsErrNotFound(err) {
				t.Errorf("remove test-owned image %s: %v", tag, err)
			}
			if _, _, err := factory.cli.ImageInspectWithRaw(cleanup, tag); err == nil {
				t.Errorf("test-owned image tag %s remains after cleanup", tag)
			} else if !client.IsErrNotFound(err) {
				t.Errorf("inspect test-owned image tag %s after cleanup: %v", tag, err)
			}
		}
	})

	limits := sandbox.Limits{CPUs: 1, MemoryMiB: 1024, Pids: 64, WorkspaceMiB: 64}
	labels := map[string]string{"agenteval.project": "harbor-integration", "agenteval.run": "integration", "agenteval.attempt": "integration-1"}
	agent, err := factory.Create(ctx, sandbox.Spec{Image: agentImage, Workdir: "/app", ImageWorkspace: true, Harbor: true, Env: []string{"TASK_VARIANT=agent"}, Limits: limits, Labels: labels}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer destroyTestSandbox(t, agent)
	assertHarborContainerConfig(t, ctx, factory, agent.(*instance), false)
	if got, err := agent.ReadFile(ctx, "initial.json"); err != nil || string(got) != `{"initial":true}` {
		t.Fatalf("image seed file=%q err=%v", got, err)
	}
	probe, err := agent.Exec(ctx, sandbox.ExecRequest{Argv: []string{"/bin/sh", "-c", "test \"$(id -u)\" = 10001 && test -f /opt/agenteval-seed/initial.json && test ! -e /tests/hidden.txt && test \"$AGENTEVAL_TEST_IMAGE_ENV\" = from-image && test \"$GOMODCACHE\" = /opt/prewarmed-go-mod && mkdir -p __pycache__ .git .cache cache"}, Workdir: ".", Timeout: 10 * time.Second})
	if err != nil || !probe.ExitKnown || probe.ExitCode != 0 {
		t.Fatalf("agent workspace probe result=%+v err=%v", probe, err)
	}
	jsonData := []byte(`{"harbor":"snapshot","ok":true}`)
	binary := make([]byte, 2<<20)
	for i := range binary {
		binary[i] = byte(i*31 + 7)
	}
	for name, data := range map[string][]byte{"output/result.json": jsonData, "output/payload.bin": binary, "run.sh": []byte("#!/bin/sh\necho harbor-run-ok\n")} {
		if err := agent.WriteFile(ctx, name, data); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	snapshot, err := agent.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	modes := agent.(sandbox.ModeSnapshotter).SnapshotModes()
	if modes["run.sh"]&0111 == 0 {
		t.Fatalf("snapshot did not preserve executable mode: %o", modes["run.sh"])
	}
	for _, name := range []string{"__pycache__", ".git", ".cache", "cache"} {
		if _, ok := snapshot[name+"/ignored"]; ok {
			t.Fatalf("cache path %s included in snapshot", name)
		}
	}
	if !bytes.Equal(snapshot["output/payload.bin"], binary) || !bytes.Equal(snapshot["output/result.json"], jsonData) {
		t.Fatal("Harbor snapshot did not preserve JSON/binary files")
	}

	verifier, err := factory.Create(ctx, sandbox.Spec{Image: verifierImage, Workdir: "/app", ImageWorkspace: true, Harbor: true, Env: []string{"TASK_VARIANT=verifier"}, Limits: limits, Labels: map[string]string{"agenteval.project": "harbor-integration", "agenteval.run": "integration", "agenteval.attempt": "integration-2"}, FileModes: modes, WritableDirs: []string{"/stage_runs"}}, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	defer destroyTestSandbox(t, verifier)
	assertHarborContainerConfig(t, ctx, factory, verifier.(*instance), true, "/stage_runs")
	for name, want := range map[string][]byte{"output/result.json": jsonData, "output/payload.bin": binary} {
		got, err := verifier.ReadFile(ctx, name)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("verifier read %s bytes=%d err=%v", name, len(got), err)
		}
	}
	run, err := verifier.Exec(ctx, sandbox.ExecRequest{Argv: []string{"./run.sh"}, Workdir: ".", Timeout: 10 * time.Second})
	if err != nil || !run.ExitKnown || run.ExitCode != 0 || run.Stdout != "harbor-run-ok\n" {
		t.Fatalf("verifier relative-workdir execution=%+v err=%v", run, err)
	}
	private, err := verifier.Exec(ctx, sandbox.ExecRequest{Argv: []string{"test", "-r", "/tests/hidden.txt"}, Workdir: "/app", Timeout: 10 * time.Second})
	if err != nil || !private.ExitKnown || private.ExitCode != 0 {
		t.Fatalf("verifier private tests probe=%+v err=%v", private, err)
	}
	if _, leaked := snapshot["../tests/hidden.txt"]; leaked {
		t.Fatal("private tests leaked into workspace snapshot")
	}

	if err := agent.Destroy(ctx); err != nil {
		t.Fatalf("destroy agent sandbox: %v", err)
	}
	if err := verifier.Destroy(ctx); err != nil {
		t.Fatalf("destroy verifier sandbox: %v", err)
	}
	for _, sb := range []*instance{agent.(*instance), verifier.(*instance)} {
		if _, err := factory.cli.ContainerInspect(ctx, sb.id); err == nil {
			t.Fatalf("owned test container %s remains after destroy", shortID(sb.id))
		} else if !client.IsErrNotFound(err) {
			t.Fatalf("inspect destroyed test container %s: %v", shortID(sb.id), err)
		}
	}
}

func assertHarborContainerConfig(t *testing.T, ctx context.Context, factory *Factory, sb *instance, hasTests bool, writableDirs ...string) {
	t.Helper()
	in, err := factory.cli.ContainerInspect(ctx, sb.id)
	if err != nil {
		t.Fatal(err)
	}
	if in.Config.User != "10001:10001" || in.Config.WorkingDir != "/app" || !in.HostConfig.ReadonlyRootfs || in.HostConfig.NetworkMode != "none" {
		t.Fatalf("Harbor sandbox identity/isolation incomplete: user=%q workdir=%q readonly=%v network=%s", in.Config.User, in.Config.WorkingDir, in.HostConfig.ReadonlyRootfs, in.HostConfig.NetworkMode)
	}
	if len(in.HostConfig.Binds) != 0 || len(in.HostConfig.VolumesFrom) != 0 || len(in.HostConfig.Mounts) != 0 || len(in.HostConfig.Tmpfs) != 3+len(writableDirs) {
		t.Fatalf("Harbor sandbox has unexpected host mounts: %+v", in.HostConfig)
	}
	for _, m := range in.Mounts {
		if m.Type != "tmpfs" {
			t.Fatalf("non-tmpfs mount: %+v", m)
		}
	}
	if in.HostConfig.Tmpfs["/app"] == "" || in.HostConfig.Tmpfs["/logs"] == "" {
		t.Fatalf("missing Harbor tmpfs mounts: %+v", in.HostConfig.Tmpfs)
	}
	for _, dir := range writableDirs {
		options := in.HostConfig.Tmpfs[dir]
		if !hasTmpfsOption(options, "size=256M") || !hasTmpfsOption(options, "uid=10001") || !hasTmpfsOption(options, "gid=10001") || !hasTmpfsOption(options, "nosuid") || !hasTmpfsOption(options, "nodev") || !hasTmpfsOption(options, "noexec") || !hasTmpfsOption(options, "mode=0755") {
			t.Fatalf("incorrect writable scratch mount %q: %q", dir, options)
		}
	}
	if len(in.HostConfig.CapDrop) == 0 || in.HostConfig.CapDrop[0] != "ALL" {
		t.Fatal("Harbor sandbox did not drop capabilities")
	}
	found := false
	for _, option := range in.HostConfig.SecurityOpt {
		if option == "no-new-privileges:true" {
			found = true
		}
	}
	if !found {
		t.Fatal("Harbor sandbox did not apply no-new-privileges")
	}
	probe := "test -d /logs && test -w /logs && test -d /app && ! test -w /stage_runs && touch /logs/verifier"
	if hasTests {
		probe = "test -r /tests/hidden.txt && test -d /logs && test -w /logs && test -d /app && test -w /stage_runs && touch /logs/verifier /stage_runs/probe"
	}
	argv := []string{"/bin/sh", "-c", probe}
	r, err := sb.Exec(ctx, sandbox.ExecRequest{Argv: argv, Workdir: ".", Timeout: 10 * time.Second})
	if err != nil || !r.ExitKnown || r.ExitCode != 0 {
		t.Fatalf("Harbor /logs and /tests visibility probe=%+v err=%v", r, err)
	}
}

func destroyTestSandbox(t *testing.T, sb sandbox.Sandbox) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := sb.Destroy(ctx); err != nil {
			t.Errorf("destroy Harbor integration sandbox: %v", err)
		}
	})
}

func buildHarborTestImages(ctx context.Context, factory *Factory, baseID string) (agentID, verifierID, agentTag, verifierTag string, err error) {
	suffix := fmt.Sprintf("%x", time.Now().UnixNano())
	agentTag = "agenteval-harbor-agent-test:" + suffix
	verifierTag = "agenteval-harbor-verifier-test:" + suffix
	var contextTar bytes.Buffer
	tw := tar.NewWriter(&contextTar)
	files := map[string]string{
		"seed/initial.json":              `{"initial":true}`,
		"seed/run.sh":                    "#!/bin/sh\necho harbor-run-ok\n",
		"seed/node_modules/pkg/index.js": "module.exports = true;\n",
		"private/hidden.txt":             "grading-only-secret\n",
		"Dockerfile.agent":               fmt.Sprintf("FROM %s\nENV AGENTEVAL_TEST_IMAGE_ENV=from-image GOMODCACHE=/opt/prewarmed-go-mod\nUSER 0\nCOPY seed/ /opt/agenteval-seed/\nRUN mkdir -p /app && cp -R /opt/agenteval-seed/. /app/ && chown -R 10001:10001 /opt/agenteval-seed /app && chmod 0755 /opt/agenteval-seed/run.sh /app/run.sh\nUSER 10001:10001\nWORKDIR /app\nENTRYPOINT [\"/bin/sleep\",\"infinity\"]\n", baseID),
		"Dockerfile.verifier":            fmt.Sprintf("FROM %s\nUSER 0\nCOPY private/ /tests/\nRUN chown -R 10001:10001 /tests\nUSER 10001:10001\nWORKDIR /app\nENTRYPOINT [\"/bin/sleep\",\"infinity\"]\n", agentTag),
	}
	for name, content := range files {
		b := []byte(content)
		if err = tw.WriteHeader(&tar.Header{Name: name, Mode: 0644, Size: int64(len(b)), Typeflag: tar.TypeReg}); err != nil {
			return
		}
		if _, err = tw.Write(b); err != nil {
			return
		}
	}
	if err = tw.Close(); err != nil {
		return
	}
	agentID, err = buildTarImage(ctx, factory.cli, contextTar.Bytes(), agentTag, "Dockerfile.agent")
	if err != nil {
		return
	}
	verifierID, err = buildTarImage(ctx, factory.cli, contextTar.Bytes(), verifierTag, "Dockerfile.verifier")
	return
}

func buildTarImage(ctx context.Context, cli *client.Client, contextTar []byte, tag, dockerfile string) (string, error) {
	resp, err := cli.ImageBuild(ctx, bytes.NewReader(contextTar), types.ImageBuildOptions{Tags: []string{tag}, Dockerfile: dockerfile, Remove: true, ForceRemove: true, Version: "1"})
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	decoder := json.NewDecoder(resp.Body)
	for {
		var event struct {
			Error       string `json:"error"`
			ErrorDetail struct {
				Message string `json:"message"`
			} `json:"errorDetail"`
		}
		if err := decoder.Decode(&event); err == io.EOF {
			break
		} else if err != nil {
			return "", err
		}
		if event.ErrorDetail.Message != "" || event.Error != "" {
			return "", fmt.Errorf("build Harbor test image: %s", strings.TrimSpace(event.ErrorDetail.Message+" "+event.Error))
		}
	}
	image, _, err := cli.ImageInspectWithRaw(ctx, tag)
	if err != nil {
		return "", err
	}
	return image.ID, nil
}
