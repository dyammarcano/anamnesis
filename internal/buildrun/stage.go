// Package buildrun stages project copies for build execution and classifies build outcomes.
// Builds never run inside the analyzed repository.
package buildrun

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

var skipDirs = map[string]bool{".git": true, ".svn": true, ".hg": true}

// Stage copies the tree at src into dst (created if needed), skipping VCS metadata. Symlinks are
// not followed and not recreated. dst must not be inside src.
func Stage(ctx context.Context, src, dst string) error {
	absSrc, err := filepath.Abs(src)
	if err != nil {
		return err
	}
	absDst, err := filepath.Abs(dst)
	if err != nil {
		return err
	}
	if rel, err := filepath.Rel(absSrc, absDst); err == nil && rel != ".." && !hasDotDot(rel) {
		return fmt.Errorf("stage: destination %s is inside source %s", absDst, absSrc)
	}
	if err := os.MkdirAll(absDst, 0o755); err != nil {
		return err
	}
	return filepath.WalkDir(absSrc, func(p string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(absSrc, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(absDst, rel)
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil // symlinks, devices
		}
		return copyFile(p, target)
	})
}

func hasDotDot(rel string) bool {
	return len(rel) >= 3 && rel[:3] == ".."+string(filepath.Separator)
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(to, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
