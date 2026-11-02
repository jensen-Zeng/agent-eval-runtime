package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jensen-Zeng/agent-eval-runtime/internal/config"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/harbor"
	"github.com/jensen-Zeng/agent-eval-runtime/internal/sandbox"
	dockersandbox "github.com/jensen-Zeng/agent-eval-runtime/internal/sandbox/docker"
)

func executeHarbor(ctx context.Context, args []string, out, errout io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(errout, "Usage: runtime harbor inspect --path TASK_DIR | harbor import --path TASK_DIR [--project . --id ID --memory-mib N --verifier-tmpfs /stage_runs --config FILE]")
		return 1
	}
	switch args[0] {
	case "inspect":
		fs := flag.NewFlagSet("harbor inspect", flag.ContinueOnError)
		fs.SetOutput(errout)
		path := fs.String("path", "", "Harbor task directory")
		if err := fs.Parse(args[1:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return 0
			}
			return 1
		}
		if fs.NArg() != 0 || strings.TrimSpace(*path) == "" {
			fmt.Fprintln(errout, "inspect requires --path TASK_DIR")
			return 1
		}
		result := harbor.Inspect(*path)
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(result); err != nil {
			fmt.Fprintln(errout, err)
			return 1
		}
		if !result.Compatible {
			return 2
		}
		return 0
	case "import":
		fs := flag.NewFlagSet("harbor import", flag.ContinueOnError)
		fs.SetOutput(errout)
		path := fs.String("path", "", "Harbor task directory")
		project := fs.String("project", ".", "AgentEval project root")
		id := fs.String("id", "", "registered task ID; defaults to metadata.instance_id")
		memory := fs.Int64("memory-mib", 0, "explicit effective sandbox memory override (1024..16384 MiB)")
		verifierTmpfs := fs.String("verifier-tmpfs", "", "comma-separated top-level scratch tmpfs directories for verifier only (maximum 4)")
		configFile := fs.String("config", "", "runtime JSON config for Docker endpoint and helper image")
		if err := fs.Parse(args[1:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return 0
			}
			return 1
		}
		if fs.NArg() != 0 || strings.TrimSpace(*path) == "" {
			fmt.Fprintln(errout, "import requires --path TASK_DIR")
			return 1
		}
		tmpfsDirs, err := parseVerifierTmpfs(*verifierTmpfs)
		if err != nil {
			fmt.Fprintf(errout, "harbor import: %v\n", err)
			return 1
		}
		if err = sandbox.ValidateWritableDirs(tmpfsDirs); err != nil {
			fmt.Fprintf(errout, "harbor import: invalid --verifier-tmpfs: %v\n", err)
			return 1
		}
		check := harbor.Inspect(*path)
		if !check.Compatible {
			fmt.Fprintf(errout, "harbor import: incompatible package: %s\n", check.Reason)
			return 1
		}
		configPath := strings.ToLower(strings.ReplaceAll(*configFile, "\\", "/"))
		if strings.Contains(configPath, "configs/ui/") || strings.HasSuffix(configPath, "/configs/ui") {
			fmt.Fprintln(errout, "harbor import: --config must not point into configs/ui")
			return 1
		}
		cfg, err := config.Load(*configFile)
		if err != nil {
			fmt.Fprintf(errout, "harbor import: load config: %v\n", err)
			return 1
		}
		buildCtx, cancel := context.WithTimeout(ctx, 2*time.Hour)
		defer cancel()
		factory, err := dockersandbox.New(buildCtx, dockersandbox.Options{Host: cfg.DockerHost, Image: cfg.Image})
		if err != nil {
			fmt.Fprintf(errout, "harbor import: Docker: %v\n", err)
			return 1
		}
		helperID, ensureErr := factory.EnsureImage(buildCtx, *project, cfg.Image)
		closeErr := factory.Close()
		if ensureErr != nil {
			fmt.Fprintf(errout, "harbor import: prepare helper image: %v\n", ensureErr)
			return 1
		}
		if closeErr != nil {
			fmt.Fprintf(errout, "harbor import: close Docker client: %v\n", closeErr)
			return 1
		}
		_, err = harbor.Import(buildCtx, harbor.ImportOptions{Path: *path, Project: *project, ID: *id, HelperImageID: helperID, DockerHost: cfg.DockerHost, MemoryMiB: *memory, VerifierTmpfs: tmpfsDirs, Stdout: out, Stderr: errout})
		if err != nil {
			fmt.Fprintf(errout, "harbor import: %v\n", err)
			return 1
		}
		return 0
	default:
		fmt.Fprintf(errout, "unknown harbor command %q\n", args[0])
		return 1
	}
}

func parseVerifierTmpfs(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
		if parts[i] == "" {
			return nil, fmt.Errorf("--verifier-tmpfs contains an empty path")
		}
	}
	return parts, nil
}
