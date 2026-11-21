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
	return a.run(ctx, scanNow, updatePending, false)
}

func (a *App) run(ctx context.Context, scanNow, updatePending <-chan struct{}, once bool) error {
	var permits chan context.Context
	if a.matchActivity != nil {
		permits = make(chan context.Context)
		watchCtx, stop := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			a.watchMatches(watchCtx, permits)
		}()
		defer func() {
			stop()
			<-done
		}()
	}

	backoff := time.Minute

	for ctx.Err() == nil {
		select {
		case <-updatePending:
			return errUpdatePending
		default:
		}

		scanCtx := ctx
		if permits != nil {
			select {
			case scanCtx = <-permits:
			case <-updatePending:
				return errUpdatePending
			case <-ctx.Done():
				continue
			}
		}

		err := a.Scan(scanCtx)
		if errors.Is(context.Cause(scanCtx), errMatchPaused) {
			continue
		}
		if once {
			return err
		}
		if ctx.Err() != nil {
			break
		}

		timer := time.NewTimer(a.scanDelay(err, &backoff))
		select {
		case <-updatePending:
			timer.Stop()
			return errUpdatePending
		case <-scanCtx.Done():
		case <-scanNow:
			a.log.Info("scan requested")
		case <-timer.C:
		}
		timer.Stop()
	}
	if once {
		return ctx.Err()
	}
	a.log.Info("stopped")
	return nil
}

// scanDelay records the result and returns the wait before the next scan.
func (a *App) scanDelay(err error, backoff *time.Duration) time.Duration {
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

	return delay
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
