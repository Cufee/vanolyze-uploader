package upload

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

// importDemos uploads the replays the game saved to its Demos folder as
// valorant/local/<match>-<sha256>.vrf, then deletes each file once the bucket
// holds it. A file the bucket already holds under an account with the same
// SHA-256 is deleted without a second copy. Recently modified files, files
// that change while uploading, symlinks and other names are left alone.
func (a *App) importDemos(ctx context.Context, idx *index) error {
	entries, err := os.ReadDir(a.demos)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}

	if err != nil {
		return err
	}

	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}

		name := entry.Name()
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !strings.EqualFold(filepath.Ext(name), ".vrf") {
			continue
		}

		id, err := uuid.Parse(strings.TrimSuffix(name, filepath.Ext(name)))
		if err != nil {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			return err
		}

		if !info.Mode().IsRegular() || info.Size() <= 0 || time.Since(info.ModTime()) < a.minimumAge {
			continue
		}

		if info.Size() > a.maxReplayBytes {
			a.log.Warn("local replay exceeds the size limit; keeping it", "file", name)

			continue
		}

		if err := a.importDemo(ctx, filepath.Join(a.demos, name), id.String(), info, idx); err != nil {
			return err
		}
	}

	return nil
}

func (a *App) importDemo(ctx context.Context, path, id string, before os.FileInfo, idx *index) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}

	defer func() {
		_ = f.Close()
	}()

	opened, err := f.Stat()
	if err != nil {
		return err
	}

	if !os.SameFile(before, opened) {
		return errors.New("local replay changed before upload; will retry")
	}

	d, err := digestFile(f)
	if err != nil {
		return err
	}

	stored, err := a.storedElsewhere(ctx, id, d, idx)
	if err != nil {
		return err
	}

	key := keyPrefix + "local/" + id + "-" + d.SHA256 + ".vrf"

	if !stored {
		a.status.activity("Uploading local replay " + id[:8])

		if err := a.put(ctx, key, f, d, map[string]string{
			"uploader": uploaderName,
			"sha256":   d.SHA256,
			"match-id": id,
		}); err != nil {
			return &storeError{
				err: err,
			}
		}

		idx.add(key, "")
		a.log.Info("archived local replay", "key", key, "bytes", d.Size)
		a.status.archived()
	}

	// Check again before deleting: the game may have rewritten the file meanwhile.
	after, err := digestFile(f)
	if err != nil {
		return err
	}

	current, err := os.Lstat(path)
	if err != nil {
		return err
	}

	if after != d || current.Size() != opened.Size() || !current.ModTime().Equal(opened.ModTime()) ||
		!os.SameFile(opened, current) {
		return errors.New("local replay changed during upload; keeping it")
	}

	if err := f.Close(); err != nil {
		return err
	}

	// The game holds a replay it is playing open; the next scan removes it.
	if err := os.Remove(path); err != nil {
		a.log.Warn("stored local replay not removed yet", "match", id, "error", err)

		return nil
	}

	a.log.Info("removed stored local replay", "match", id)

	return nil
}

// storedElsewhere reports whether the bucket holds these exact bytes for the
// match under another key.
func (a *App) storedElsewhere(ctx context.Context, id string, d digest, idx *index) (bool, error) {
	for _, key := range idx.matches[id] {
		object, err := a.store.Head(ctx, key)
		if err != nil {
			return false, &storeError{
				err: err,
			}
		}

		if d.matches(object) {
			return true, nil
		}
	}

	return false, nil
}
