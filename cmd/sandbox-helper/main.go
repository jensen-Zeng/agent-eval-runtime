// sandbox-helper is the only file API exposed to the controller. os.Root keeps
// operations confined even when an untrusted task creates symlinks concurrently.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const (
	maxFile  = 1 << 20
	maxTotal = 50 << 20
	maxFiles = 1000
)

type Request struct {
	Op    string `json:"op"`
	Path  string `json:"path,omitempty"`
	Data  []byte `json:"data,omitempty"`
	Patch string `json:"patch,omitempty"`
}
type Response struct {
	OK    bool              `json:"ok"`
	Data  []byte            `json:"data,omitempty"`
	Paths []string          `json:"paths,omitempty"`
	Files map[string][]byte `json:"files,omitempty"`
	Error string            `json:"error,omitempty"`
}

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--oom-probe" {
		var retained [][]byte
		for {
			block := make([]byte, 8<<20)
			for i := 0; i < len(block); i += 4096 {
				block[i] = 1
			}
			retained = append(retained, block)
		}
	}
	if len(os.Args) == 2 && os.Args[1] == "--seed" {
		root, err := os.OpenRoot("/workspace")
		if err == nil {
			defer root.Close()
			err = seedFromTar(root, os.Stdin)
		}
		if err != nil {
			respond(Response{Error: err.Error()})
		} else {
			respond(Response{OK: true})
		}
		return
	}
	var req Request
	if err := json.NewDecoder(bufio.NewReader(os.Stdin)).Decode(&req); err != nil {
		respond(Response{Error: err.Error()})
		return
	}
	r, e := os.OpenRoot("/workspace")
	if e != nil {
		respond(Response{Error: e.Error()})
		return
	}
	defer r.Close()
	out, e := handle(r, req)
	if e != nil {
		out = Response{Error: e.Error()}
	} else {
		out.OK = true
	}
	respond(out)
}
func respond(v Response) { _ = json.NewEncoder(os.Stdout).Encode(v) }

func clean(p string) (string, error) {
	if p == "" {
		p = "."
	}
	if strings.ContainsRune(p, 0) || strings.Contains(p, `\`) || filepath.IsAbs(p) || (len(p) >= 2 && p[1] == ':') {
		return "", fmt.Errorf("invalid workspace path")
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".." {
			return "", fmt.Errorf("path escapes workspace")
		}
	}
	p = filepath.ToSlash(filepath.Clean(p))
	if p == "." {
		return ".", nil
	}
	if p == ".." || strings.HasPrefix(p, "../") {
		return "", fmt.Errorf("path escapes workspace")
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".git" {
			return "", fmt.Errorf(".git paths are not allowed in patches")
		}
	}
	return p, nil
}
func handle(root *os.Root, q Request) (Response, error) {
	switch q.Op {
	case "read":
		p, e := clean(q.Path)
		if e != nil {
			return Response{}, e
		}
		f, e := root.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if e != nil {
			return Response{}, e
		}
		defer f.Close()
		st, e := f.Stat()
		if e != nil {
			return Response{}, e
		}
		if !st.Mode().IsRegular() {
			return Response{}, fmt.Errorf("not a regular file")
		}
		b, e := io.ReadAll(io.LimitReader(f, maxFile+1))
		if e != nil {
			return Response{}, e
		}
		if len(b) > maxFile {
			return Response{}, fmt.Errorf("file exceeds 1 MiB")
		}
		return Response{Data: b}, nil
	case "write":
		p, e := clean(q.Path)
		if e != nil {
			return Response{}, e
		}
		if len(q.Data) > maxFile {
			return Response{}, fmt.Errorf("file exceeds 1 MiB")
		}
		if e = root.MkdirAll(filepath.Dir(p), 0755); e != nil {
			return Response{}, e
		}
		f, e := root.OpenFile(p, os.O_WRONLY|os.O_CREATE|syscall.O_NONBLOCK, 0644)
		if e != nil {
			return Response{}, e
		}
		st, e := f.Stat()
		if e != nil {
			_ = f.Close()
			return Response{}, e
		}
		if !st.Mode().IsRegular() {
			_ = f.Close()
			return Response{}, fmt.Errorf("not a regular file")
		}
		if e = f.Truncate(0); e != nil {
			_ = f.Close()
			return Response{}, e
		}
		_, e = f.Write(q.Data)
		ce := f.Close()
		if e != nil {
			return Response{}, e
		}
		return Response{}, ce
	case "list", "snapshot":
		p, e := clean(q.Path)
		if e != nil {
			return Response{}, e
		}
		out := Response{Files: map[string][]byte{}}
		var total int64
		var totalRead int64
		count := 0
		dirs := 0
		var visit func(string) error
		visit = func(dir string) error {
			f, e := root.Open(dir)
			if e != nil {
				return e
			}
			entries, e := f.ReadDir(-1)
			_ = f.Close()
			if e != nil {
				return e
			}
			for _, d := range entries {
				full := filepath.Join(dir, d.Name())
				info, e := d.Info()
				if e != nil {
					return e
				}
				if d.Type()&os.ModeSymlink != 0 || !info.Mode().IsRegular() && !info.IsDir() {
					return fmt.Errorf("unsupported file type: %s", full)
				}
				if info.IsDir() {
					dirs++
				} else {
					count++
				}
				if dirs > 2000 {
					return fmt.Errorf("workspace directory limit exceeded")
				}
				if count > maxFiles {
					return fmt.Errorf("workspace exceeds 1000 files")
				}
				rel := filepath.ToSlash(full)
				out.Paths = append(out.Paths, rel)
				if info.IsDir() {
					if e = visit(full); e != nil {
						return e
					}
					continue
				}
				if info.Size() > maxFile {
					return fmt.Errorf("file exceeds 1 MiB: %s", rel)
				}
				total += info.Size()
				if total > maxTotal {
					return fmt.Errorf("workspace exceeds 50 MiB")
				}
				if q.Op == "snapshot" {
					file, e := root.OpenFile(full, os.O_RDONLY|syscall.O_NONBLOCK, 0)
					if e != nil {
						return e
					}
					st, e := file.Stat()
					if e != nil {
						_ = file.Close()
						return e
					}
					if !st.Mode().IsRegular() {
						_ = file.Close()
						return fmt.Errorf("not a regular file: %s", rel)
					}
					b, e := io.ReadAll(io.LimitReader(file, maxFile+1))
					_ = file.Close()
					if e != nil {
						return e
					}
					if len(b) > maxFile {
						return fmt.Errorf("file exceeds 1 MiB: %s", rel)
					}
					totalRead += int64(len(b))
					if totalRead > maxTotal {
						return fmt.Errorf("workspace exceeds 50 MiB")
					}
					out.Files[rel] = b
				}
			}
			return nil
		}
		if p == "." {
			p = "."
		}
		e = visit(p)
		if e != nil {
			return Response{}, e
		}
		if q.Op == "list" {
			out.Files = nil
		}
		return out, nil
	case "patch":
		if strings.TrimSpace(q.Patch) == "" {
			return Response{}, fmt.Errorf("empty patch")
		}
		if e := validatePatch(q.Patch); e != nil {
			return Response{}, e
		} // caller invokes git apply in the container after validation
		return Response{}, nil
	default:
		return Response{}, fmt.Errorf("unknown operation")
	}
}
func validatePatch(s string) error {
	scanner := bufio.NewScanner(strings.NewReader(s))
	diffA, diffB := "", ""
	sawDiff, sawOld, sawNew := false, false, false
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, `"`) && (strings.HasPrefix(line, "diff --git ") || strings.HasPrefix(line, "--- ") || strings.HasPrefix(line, "+++ ")) {
			return fmt.Errorf("quoted patch paths are not allowed")
		}
		if strings.HasPrefix(line, "diff --git ") {
			if sawDiff && (!sawOld || !sawNew) {
				return fmt.Errorf("patch diff is missing ---/+++ headers")
			}
			fields := strings.Fields(strings.TrimPrefix(line, "diff --git "))
			if len(fields) != 2 || !strings.HasPrefix(fields[0], "a/") || !strings.HasPrefix(fields[1], "b/") {
				return fmt.Errorf("invalid git diff header")
			}
			diffA, diffB = strings.TrimPrefix(fields[0], "a/"), strings.TrimPrefix(fields[1], "b/")
			if diffA != "/dev/null" && diffB != "/dev/null" && diffA != diffB {
				return fmt.Errorf("renamed diff paths are not supported")
			}
			if diffA != "/dev/null" {
				if _, e := clean(diffA); e != nil {
					return e
				}
			}
			if diffB != "/dev/null" {
				if _, e := clean(diffB); e != nil {
					return e
				}
			}
			sawDiff, sawOld, sawNew = true, false, false
			continue
		}
		if strings.HasPrefix(line, "rename ") || strings.HasPrefix(line, "copy ") || strings.Contains(line, " file mode ") || strings.HasPrefix(line, "GIT binary patch") || strings.HasPrefix(line, "Binary files ") || strings.HasPrefix(line, "similarity index ") || strings.HasPrefix(line, "dissimilarity index ") {
			return fmt.Errorf("unsupported patch metadata")
		}
		if strings.HasPrefix(line, "--- ") {
			if !sawDiff || sawOld {
				return fmt.Errorf("unexpected old-file patch header")
			}
			parts := strings.Fields(strings.TrimSpace(strings.TrimPrefix(line, "--- ")))
			if len(parts) == 0 {
				return fmt.Errorf("missing old-file path")
			}
			p := parts[0]
			if p != "/dev/null" {
				if !strings.HasPrefix(p, "a/") {
					return fmt.Errorf("invalid old-file path")
				}
				p = strings.TrimPrefix(p, "a/")
				if _, e := clean(p); e != nil {
					return e
				}
			}
			if p != "/dev/null" && p != diffA {
				return fmt.Errorf("old-file path does not match diff header")
			}
			sawOld = true
			continue
		}
		if strings.HasPrefix(line, "+++ ") {
			if !sawOld || sawNew {
				return fmt.Errorf("unexpected new-file patch header")
			}
			parts := strings.Fields(strings.TrimSpace(strings.TrimPrefix(line, "+++ ")))
			if len(parts) == 0 {
				return fmt.Errorf("missing new-file path")
			}
			p := parts[0]
			if p != "/dev/null" {
				if !strings.HasPrefix(p, "b/") {
					return fmt.Errorf("invalid new-file path")
				}
				p = strings.TrimPrefix(p, "b/")
				if _, e := clean(p); e != nil {
					return e
				}
			}
			if p != "/dev/null" && p != diffB {
				return fmt.Errorf("new-file path does not match diff header")
			}
			sawNew = true
			continue
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if !sawDiff || !sawOld || !sawNew {
		return fmt.Errorf("patch must contain standard unified diffs")
	}
	return nil
}
