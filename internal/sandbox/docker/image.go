package docker

import (
	"archive/tar"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
)

func EnsureImage(ctx context.Context, projectDir, tag string) (string, error) {
	f, err := New(ctx, Options{})
	if err != nil {
		return "", err
	}
	defer f.Close()
	return f.EnsureImage(ctx, projectDir, tag)
}

func ensureImage(ctx context.Context, cli *client.Client, projectDir, tag string) (string, error) {
	if tag == "" {
		tag = "agenteval-sandbox:v0.1"
	}
	root, err := filepath.Abs(projectDir)
	if err != nil {
		return "", err
	}
	pr, pw := io.Pipe()
	defer pr.Close()
	go func() {
		tw := tar.NewWriter(pw)
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, e error) error {
			if e != nil {
				return e
			}
			rel, e := filepath.Rel(root, path)
			if e != nil {
				return e
			}
			if rel == "." {
				return nil
			}
			rel = filepath.ToSlash(rel)
			if d.IsDir() {
				if rel == ".git" || strings.HasPrefix(rel, ".git/") || rel == ".setup" || strings.HasPrefix(rel, ".setup/") {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.Contains(rel, "node_modules/") || strings.Contains(rel, "vendor/") {
				return nil
			}
			include := rel == "deployments/Dockerfile.sandbox" || rel == "go.mod" || strings.HasPrefix(rel, "cmd/sandbox-helper/") && strings.HasSuffix(rel, ".go") && !strings.HasSuffix(rel, "_test.go")
			if include {
				info, e := d.Info()
				if e != nil {
					return e
				}
				if !info.Mode().IsRegular() {
					return fmt.Errorf("refusing non-regular trusted build input: %s", rel)
				}
				h := &tar.Header{Name: rel, Mode: int64(info.Mode().Perm()), Size: info.Size(), ModTime: info.ModTime()}
				if e = tw.WriteHeader(h); e != nil {
					return e
				}
				f, e := os.Open(path)
				if e != nil {
					return e
				}
				_, e = io.Copy(tw, f)
				ce := f.Close()
				if e != nil {
					return e
				}
				return ce
			}
			return nil
		})
		ce := tw.Close()
		if err == nil {
			err = ce
		}
		_ = pw.CloseWithError(err)
	}()
	resp, err := cli.ImageBuild(ctx, pr, types.ImageBuildOptions{Tags: []string{tag}, Dockerfile: "deployments/Dockerfile.sandbox", Remove: true, ForceRemove: true, Version: "1"})
	if err != nil {
		return "", fmt.Errorf("build sandbox image: %w", err)
	}
	defer resp.Body.Close()
	decoder := json.NewDecoder(resp.Body)
	for {
		var msg struct {
			Error       string `json:"error"`
			ErrorDetail struct {
				Message string `json:"message"`
			} `json:"errorDetail"`
		}
		e := decoder.Decode(&msg)
		if e == io.EOF {
			break
		}
		if e != nil {
			return "", fmt.Errorf("read Docker build output: %w", e)
		}
		message := msg.ErrorDetail.Message
		if message == "" {
			message = msg.Error
		}
		if message != "" {
			if len(message) > 4096 {
				message = message[:4096]
			}
			return "", fmt.Errorf("Docker image build failed: %s", message)
		}
	}
	ins, _, e := cli.ImageInspectWithRaw(ctx, tag)
	if e != nil {
		return "", fmt.Errorf("inspect built image: %w", e)
	}
	return ins.ID, nil
}
