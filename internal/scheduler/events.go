package scheduler

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/jensen-Zeng/agent-eval-runtime/internal/jobs"
)

const (
	maxTraceBytes     = 10 << 20
	maxTraceLine      = 256 << 10
	defaultEventLimit = 100
	maxEventLimit     = 1000
)

func (s *Service) Events(ctx context.Context, id string, after, limit int) (jobs.EventPage, error) {
	if after < 0 {
		return jobs.EventPage{}, fmt.Errorf("after sequence cannot be negative")
	}
	if limit <= 0 {
		limit = defaultEventLimit
	}
	if limit > maxEventLimit {
		limit = maxEventLimit
	}
	job, err := s.store.Get(ctx, id)
	if err != nil {
		return jobs.EventPage{}, err
	}
	page := jobs.EventPage{Events: []json.RawMessage{}, NextSequence: after}
	if !idPattern.MatchString(job.RunID) || job.AttemptID != job.RunID+"-1" {
		return page, nil
	}
	root, err := os.OpenRoot(s.artifactsDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return page, nil
		}
		return jobs.EventPage{}, fmt.Errorf("open artifacts root: %w", err)
	}
	defer root.Close()
	rel := filepath.Join(job.RunID, "attempts", job.AttemptID, "trace.jsonl")
	if err := rejectSymlinkComponents(root, rel); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return page, nil
		}
		return jobs.EventPage{}, err
	}
	f, err := root.Open(rel)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return page, nil
		}
		return jobs.EventPage{}, fmt.Errorf("open run trace: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return jobs.EventPage{}, err
	}
	if !info.Mode().IsRegular() {
		return jobs.EventPage{}, fmt.Errorf("run trace is not a regular file")
	}
	if info.Size() > maxTraceBytes+maxTraceLine {
		return jobs.EventPage{}, fmt.Errorf("run trace exceeds read limit")
	}
	reader := bufio.NewReaderSize(io.LimitReader(f, maxTraceBytes+maxTraceLine+1), 4096)
	for {
		line, readErr := readTraceLine(reader)
		if readErr == io.EOF {
			// The run process may still be appending the final line.
			break
		}
		if readErr != nil {
			return jobs.EventPage{}, readErr
		}
		if len(line) > maxTraceLine+1 {
			return jobs.EventPage{}, fmt.Errorf("trace event exceeds line limit")
		}
		if err := ctx.Err(); err != nil {
			return jobs.EventPage{}, err
		}
		line = line[:len(line)-1]
		if len(line) == 0 {
			continue
		}
		if len(line) > maxTraceLine {
			return jobs.EventPage{}, fmt.Errorf("trace event exceeds line limit")
		}
		var event struct {
			Sequence int `json:"sequence"`
		}
		if err := json.Unmarshal(line, &event); err != nil {
			return jobs.EventPage{}, fmt.Errorf("decode trace event: %w", err)
		}
		if event.Sequence < 1 {
			return jobs.EventPage{}, fmt.Errorf("trace event has invalid sequence")
		}
		if event.Sequence <= after {
			continue
		}
		if len(page.Events) < limit {
			page.Events = append(page.Events, append(json.RawMessage(nil), line...))
			page.NextSequence = event.Sequence
		} else {
			page.HasMore = true
			break
		}
	}
	return page, nil
}

func rejectSymlinkComponents(root *os.Root, relative string) error {
	current := ""
	parts := strings.Split(filepath.ToSlash(relative), "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("invalid trace path")
		}
		if current == "" {
			current = part
		} else {
			current = filepath.Join(current, part)
		}
		info, err := root.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("trace path contains a symlink")
		}
	}
	return nil
}
