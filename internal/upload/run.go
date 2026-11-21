package upload

import (
	"context"
	"errors"
	"os"
	"sync"
	"time"

	"github.com/Cufee/vanolyze-uploader/internal/riot"
)

// Run scans every ScanInterval until ctx ends, sooner when scanNow fires.
// Failed scans retry with backoff, and a Riot rate limit waits out its
// Retry-After.
func (a *App) Run(ctx context.Context, scanNow, updatePending <-chan struct{}) error {
	backoff := time.Minute

	for {
		select {
		case <-updatePending:
			return errUpdatePending
		default:
		}

		delay, stopped := a.scanOnce(ctx, &backoff)
		if stopped {
			a.log.Info("stopped")

			return nil
		}

		timer := time.NewTimer(delay)

		select {
		case <-updatePending:
			timer.Stop()
			return errUpdatePending
		case <-ctx.Done():
			timer.Stop()
		case <-scanNow:
			timer.Stop()
			a.log.Info("scan requested")
		case <-timer.C:
		}
	}
}

// scanOnce scans and returns the wait before the next scan, or stopped when
// ctx ended.
func (a *App) scanOnce(ctx context.Context, backoff *time.Duration) (time.Duration, bool) {
	err := a.Scan(ctx)
	if ctx.Err() != nil {
		return 0, true
	}

	delay := a.cfg.ScanInterval

	var (
		rateLimited *riot.RateLimitedError
		connect     *ConnectError
	)

	switch {
	case err == nil:
		*backoff = time.Minute

		a.log.Info("scan complete")
	case errors.As(err, &rateLimited):
		delay = max(time.Until(rateLimited.Until), time.Minute)

		a.log.Warn("riot rate limited; waiting", "retry_in", delay.Round(time.Second).String())
	case errors.As(err, &connect):
		delay = min(delay, 2*time.Minute)

		a.log.Info("waiting for Riot", "reason", a.cfg.Redact(err.Error()), "retry_in", delay.String())
	default:
		delay = *backoff
		*backoff = min(*backoff*2, 15*time.Minute)

		a.log.Warn("scan incomplete; will retry", "error", a.cfg.Redact(err.Error()), "retry_in", delay.String())
	}

	errorText := ""
	if err != nil {
		errorText = a.cfg.Redact(err.Error())
	}

	a.status.waiting(time.Now().Add(delay), err, errorText)

	return delay, false
}

// RotatingLog is a log file bounded to 2 MiB plus one rotated file.
type RotatingLog struct {
	mu   sync.Mutex
	path string
	file *os.File
	size int64
}

func OpenRotatingLog(path string) (*RotatingLog, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}

	info, err := f.Stat()
	if err != nil {
		_ = f.Close()

		return nil, err
	}

	return &RotatingLog{
		path: path,
		file: f,
		size: info.Size(),
	}, nil
}

func (l *RotatingLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.size+int64(len(p)) > 2<<20 {
		if err := l.file.Close(); err != nil {
			return 0, err
		}

		if err := os.Remove(l.path + ".1"); err != nil && !os.IsNotExist(err) {
			return 0, err
		}

		if err := os.Rename(l.path, l.path+".1"); err != nil {
			return 0, err
		}

		f, err := os.OpenFile(l.path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			return 0, err
		}

		l.file, l.size = f, 0
	}

	n, err := l.file.Write(p)
	l.size += int64(n)

	return n, err
}

func (l *RotatingLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.file.Close()
}
