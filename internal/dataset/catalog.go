// Package dataset discovers and materializes operator-owned task datasets.
// It deliberately reads only explicitly selected prompt and source files; grader
// material and prior submissions stay outside the agent-facing source tree.
package dataset

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	CatalogSchemaVersion = 1
	MaxCatalogBytes      = 2 << 20
	MaxCatalogItems      = 1000
	MaxPromptBytes       = 32 << 10
	MaxPromptFiles       = 32
)

var (
	keyPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	taskPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,79}$`)
	shaPattern  = regexp.MustCompile(`^[a-fA-F0-9]{64}$`)
)

// Catalog is a portable index. Root is informational; callers pass the
// authoritative root to Resolve, ReadPrompts, and MaterializeSource.
type Catalog struct {
	SchemaVersion int    `json:"schema_version"`
	Name          string `json:"name"`
	Root          string `json:"root"`
	Adapter       string `json:"adapter"`
	Items         []Item `json:"items"`
}

// Item describes one task package and keeps its trusted grader path separate
// from the source and prompt paths that may be exposed to an agent.
type Item struct {
	Key            string            `json:"key"`
	TaskID         string            `json:"task_id"`
	PackagePath    string            `json:"package_path,omitempty"`
	Source         string            `json:"source"`
	SourceMember   string            `json:"source_member,omitempty"`
	SourceExcludes []string          `json:"source_excludes,omitempty"`
	PromptFiles    []string          `json:"prompt_files"`
	PromptMembers  []string          `json:"prompt_members,omitempty"`
	GraderDir      string            `json:"grader_dir,omitempty"`
	GraderEntry    string            `json:"grader_entry,omitempty"`
	RequiredTests  []string          `json:"required_tests,omitempty"`
	AllowedFiles   []string          `json:"allowed_files,omitempty"`
	State          string            `json:"state"`
	Notes          []string          `json:"notes,omitempty"`
	ExpectedSHA256 map[string]string `json:"expected_sha256,omitempty"`
	Metadata       map[string]string `json:"metadata,omitempty"`
}

// Load strictly decodes an operator-authored catalog and rejects unknown
// fields, invalid relative paths, duplicate keys, and oversized documents.
func Load(file string) (Catalog, error) {
	var c Catalog
	st, err := os.Lstat(file)
	if err != nil || st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular() {
		return c, fmt.Errorf("catalog must be a regular non-symlink file")
	}
	if st.Size() < 0 || st.Size() > MaxCatalogBytes {
		return c, fmt.Errorf("catalog exceeds %d bytes", MaxCatalogBytes)
	}
	f, err := os.Open(file)
	if err != nil {
		return c, err
	}
	data, readErr := io.ReadAll(io.LimitReader(f, MaxCatalogBytes+1))
	closeErr := f.Close()
	if readErr != nil {
		return c, readErr
	}
	if closeErr != nil {
		return c, closeErr
	}
	if len(data) > MaxCatalogBytes {
		return c, fmt.Errorf("catalog exceeds %d bytes", MaxCatalogBytes)
	}
	if !utf8.Valid(data) || bytes.HasPrefix(data, []byte{0xef, 0xbb, 0xbf}) {
		return c, fmt.Errorf("catalog must be strict UTF-8 without a BOM")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return c, fmt.Errorf("decode catalog: %w", err)
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return c, fmt.Errorf("catalog must contain one JSON value")
	}
	if err := validateCatalog(c); err != nil {
		return c, err
	}
	if !filepath.IsAbs(c.Root) {
		return c, fmt.Errorf("catalog root must be absolute")
	}
	resolvedRoot, err := realRoot(c.Root)
	if err != nil {
		return c, fmt.Errorf("catalog root: %w", err)
	}
	c.Root = resolvedRoot
	return c, nil
}

func validateCatalog(c Catalog) error {
	if c.SchemaVersion != CatalogSchemaVersion || strings.TrimSpace(c.Name) == "" || len(c.Name) > 128 || strings.TrimSpace(c.Root) == "" || len(c.Root) > 4096 || strings.TrimSpace(c.Adapter) == "" || len(c.Adapter) > 64 {
		return fmt.Errorf("invalid catalog identity or schema version")
	}
	if len(c.Items) > MaxCatalogItems {
		return fmt.Errorf("catalog item limit exceeded")
	}
	keys := make(map[string]struct{}, len(c.Items))
	for i, item := range c.Items {
		if !keyPattern.MatchString(item.Key) || !taskPattern.MatchString(item.TaskID) {
			return fmt.Errorf("item %d has an invalid key or task_id", i)
		}
		if _, ok := keys[item.Key]; ok {
			return fmt.Errorf("duplicate catalog key %q", item.Key)
		}
		keys[item.Key] = struct{}{}
		if (item.Source != "" && !validRelative(item.Source, false)) || (item.Source == "" && item.State != "incompatible") || (item.PackagePath != "" && !validRelative(item.PackagePath, true)) || (item.SourceMember != "" && !validMember(item.SourceMember)) {
			return fmt.Errorf("item %q has an invalid source/package path", item.Key)
		}
		if !validState(item.State) {
			return fmt.Errorf("item %q has an invalid state", item.Key)
		}
		if item.GraderDir != "" && !validRelative(item.GraderDir, false) {
			return fmt.Errorf("item %q has an invalid grader_dir", item.Key)
		}
		if item.GraderEntry != "" && !validRelative(item.GraderEntry, false) {
			return fmt.Errorf("item %q has an invalid grader_entry", item.Key)
		}
		if len(item.PromptFiles) > MaxPromptFiles || len(item.PromptMembers) > MaxPromptFiles || len(item.SourceExcludes) > 32 || len(item.RequiredTests) > 100 || len(item.AllowedFiles) > 100 || len(item.Notes) > 64 || len(item.ExpectedSHA256) > 1100 || len(item.Metadata) > 128 {
			return fmt.Errorf("item %q exceeds a catalog field limit", item.Key)
		}
		for _, p := range item.PromptFiles {
			if !validRelative(p, false) {
				return fmt.Errorf("item %q has an invalid prompt path", item.Key)
			}
		}
		for _, p := range item.PromptMembers {
			if !validPromptMember(p) {
				return fmt.Errorf("item %q has an invalid prompt member", item.Key)
			}
		}
		for _, p := range item.SourceExcludes {
			if !validMember(p) {
				return fmt.Errorf("item %q has an invalid source exclusion", item.Key)
			}
		}
		for _, p := range item.AllowedFiles {
			if !validRelative(p, false) {
				return fmt.Errorf("item %q has an invalid allowed file", item.Key)
			}
		}
		allowedExpected := map[string]struct{}{}
		if item.Source != "" {
			allowedExpected[item.Source] = struct{}{}
			if item.SourceMember != "" {
				allowedExpected[item.Source+"!"+item.SourceMember] = struct{}{}
			}
		}
		for _, p := range item.PromptFiles {
			allowedExpected[p] = struct{}{}
		}
		if item.GraderDir != "" {
			for p := range item.ExpectedSHA256 {
				if strings.HasPrefix(p, item.GraderDir+"/") {
					allowedExpected[p] = struct{}{}
				}
			}
		}
		for p, sum := range item.ExpectedSHA256 {
			if _, allowed := allowedExpected[p]; !allowed || !shaPattern.MatchString(sum) {
				return fmt.Errorf("item %q has an invalid expected SHA-256 entry", item.Key)
			}
		}
		for k, v := range item.Metadata {
			if len(k) == 0 || len(k) > 128 || len(v) > 4096 {
				return fmt.Errorf("item %q has invalid metadata", item.Key)
			}
		}
		for _, note := range item.Notes {
			if len(note) > 1024 {
				return fmt.Errorf("item %q has an oversized note", item.Key)
			}
		}
	}
	return nil
}

func validState(s string) bool {
	switch s {
	case "candidate", "missing_grader", "ambiguous", "incompatible":
		return true
	default:
		return false
	}
}

func validMember(p string) bool {
	return !strings.Contains(p, "!") && validRelative(strings.TrimSuffix(p, "/"), false)
}

func validPromptMember(p string) bool {
	if strings.Count(p, "!") == 0 {
		return validMember(p)
	}
	if strings.Count(p, "!") != 1 {
		return false
	}
	parts := strings.SplitN(p, "!", 2)
	return validMember(parts[0]) && validMember(parts[1])
}

func validRelative(p string, allowDot bool) bool {
	if p == "." {
		return allowDot
	}
	if p == "" || len(p) > 4096 || strings.ContainsAny(p, `\:`) || strings.HasPrefix(p, "/") || filepath.IsAbs(p) || filepath.VolumeName(p) != "" {
		return false
	}
	for _, r := range p {
		if r == 0 || r < 0x20 || r == 0x7f {
			return false
		}
	}
	if filepath.ToSlash(filepath.Clean(filepath.FromSlash(p))) != p || p == ".." || strings.HasPrefix(p, "../") {
		return false
	}
	for _, part := range strings.Split(p, "/") {
		if part == "" || part == "." || part == ".." || strings.HasPrefix(part, "._") || strings.EqualFold(part, "__MACOSX") {
			return false
		}
		device := strings.ToUpper(strings.TrimSuffix(part, filepath.Ext(part)))
		switch device {
		case "CON", "PRN", "AUX", "NUL", "COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9", "LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
			return false
		}
	}
	return true
}
