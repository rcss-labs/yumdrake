package artifacts

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"
)

// Retention is how long config copies are kept; they are deleted after 30 days
// in every setup.
const Retention = 30 * 24 * time.Hour

// Sweep removes regular files under dir whose modification time is before
// now-olderThan, including temporary files left by an interrupted write, and
// then removes the directories it leaves empty. Every operation goes through
// an os.Root, so nothing outside dir is touched even through a symbolic link;
// links and other non-regular files are never followed or removed, and dir
// itself is kept. A dir that does not exist is not an error.
func Sweep(ctx context.Context, dir string, olderThan time.Duration, now time.Time) (int, error) {
	root, err := os.OpenRoot(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("artifacts: open %s: %w", dir, err)
	}
	defer func() { _ = root.Close() }()

	fsys := root.FS()
	cutoff := now.Add(-olderThan)
	var dirs []string
	removed := 0
	err = fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() {
			if name != "." {
				dirs = append(dirs, name)
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.ModTime().Before(cutoff) {
			return nil
		}
		if err := root.Remove(name); err != nil {
			return fmt.Errorf("artifacts: remove %s: %w", name, err)
		}
		removed++
		return nil
	})
	if err != nil {
		return removed, fmt.Errorf("artifacts: sweep %s: %w", dir, err)
	}
	// Walk order puts parents before children, so reverse it to empty the deepest first.
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := removeIfEmpty(ctx, root, fsys, dirs[i]); err != nil {
			return removed, err
		}
	}
	return removed, nil
}

func removeIfEmpty(ctx context.Context, root *os.Root, fsys fs.FS, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	entries, err := fs.ReadDir(fsys, name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("artifacts: read %s: %w", name, err)
	}
	if len(entries) > 0 {
		return nil
	}
	if err := root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("artifacts: remove %s: %w", name, err)
	}
	return nil
}
