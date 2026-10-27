package main

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
)

func seedFromTar(root *os.Root, input io.Reader) error {
	tr := tar.NewReader(io.LimitReader(input, maxTotal+(8<<20)))
	total := int64(0)
	files, entries := 0, 0
	seen := map[string]bool{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		entries++
		if entries > 3000 {
			return fmt.Errorf("seed entry limit")
		}
		name := strings.TrimSuffix(h.Name, "/")
		safe, err := clean(name)
		if err != nil || safe == "." || safe != name || strings.ContainsAny(name, "\r\n\t:") || strings.HasPrefix(name, "-") {
			return fmt.Errorf("invalid seed path")
		}
		folded := strings.ToLower(name)
		if seen[folded] {
			return fmt.Errorf("duplicate seed path")
		}
		seen[folded] = true
		if h.Uid != 10001 || h.Gid != 10001 {
			return fmt.Errorf("seed ownership policy")
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if h.Mode != 0755 {
				return fmt.Errorf("seed directory mode policy")
			}
			if h.Size != 0 {
				return fmt.Errorf("directory has data")
			}
			if err := root.MkdirAll(safe, 0755); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if h.Mode != 0644 {
				return fmt.Errorf("seed regular mode policy")
			}
			files++
			total += h.Size
			if files > maxFiles || h.Size < 0 || h.Size > maxFile || total > maxTotal {
				return fmt.Errorf("seed size limit")
			}
			if err := root.MkdirAll(path.Dir(safe), 0755); err != nil {
				return err
			}
			f, err := root.OpenFile(safe, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
			if err != nil {
				return err
			}
			_, err = io.CopyN(f, tr, h.Size)
			closeErr := f.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return closeErr
			}
		default:
			return fmt.Errorf("seed links and special entries forbidden")
		}
	}
}
