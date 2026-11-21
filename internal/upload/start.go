package upload

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/gofrs/flock"

	"github.com/Cufee/vanolyze-uploader/internal/config"
	"github.com/Cufee/vanolyze-uploader/internal/release"
	"github.com/Cufee/vanolyze-uploader/internal/riot"
	"github.com/Cufee/vanolyze-uploader/internal/source"
)

const riotRequestGap = 1500 * time.Millisecond

type Options struct {
	ConfigPath string
	DemosDir   string
	Once       bool
	Probe      bool
	Headless   bool
	SelfCheck  bool
	Output     io.Writer
	Errors     io.Writer
}

// Run loads configuration and runs the selected uploader mode. Self-check and
// setup begin before any bucket client, replay folder or Riot session is opened.
func Run(ctx context.Context, opts Options) error {
	paths, err := config.FindUploadPaths(opts.ConfigPath)
	if err != nil {
		return err
	}

	if !opts.Probe || opts.SelfCheck {
		if _, err := config.CreateUpload(paths); err != nil {
			return err
		}
	}

	protector := config.PlatformSecretProtector()

	cfg, err := config.LoadUpload(paths, protector)
	if err != nil {
		if !opts.Probe && !opts.SelfCheck {
			_ = withData(paths.DataLocation(), nil, func(_ string, logger *slog.Logger) error {
				logger.Error("load config", "error", err)

				return nil
			})
		}

		return err
	}

	storageErr := cfg.ValidateStorage()
	needsSetup := errors.Is(storageErr, config.ErrUploadSetup)

	if opts.SelfCheck {
		if needsSetup {
			return nil
		}

		return storageErr
	}

	limiter := &riot.Limiter{
		Gap: riotRequestGap,
	}

	if opts.Probe {
		client, err := riot.Connect(ctx, limiter)
		if err != nil {
			return err
		}

		defer client.Close()

		return Probe(ctx, opts.Output, client, cfg.Friends)
	}

	uploadCtx, stop := context.WithCancel(ctx)
	defer stop()

	var stderr io.Writer
	if opts.Headless || opts.Once {
		stderr = opts.Errors
	}

	var monitor *updateMonitor
	err = withData(cfg.DataDir, stderr, func(logPath string, logger *slog.Logger) error {
		if storageErr != nil && (!needsSetup || opts.Once) {
			logger.Error("load storage settings", "error", storageErr)

			return storageErr
		}

		if paths.Managed && runtime.GOOS == "windows" && !opts.Headless && !opts.Once {
			if err := registerAutostart(); err != nil {
				logger.Warn("register autostart", "error", err)
			}
		}

		status := NewStatus()
		if needsSetup {
			status.setup(cfg.Server, logPath)
		} else {
			status.ready(logPath)
		}

		var updatePending <-chan struct{}
		var checkNow chan<- struct{}
		monitor = newUpdateMonitor(paths, opts)
		if monitor != nil {
			updatePending, checkNow = monitor.pending, monitor.check
			updateCtx, cancelUpdates := context.WithCancel(uploadCtx)
			go monitor.run(updateCtx, logger, status)
			defer func() {
				cancelUpdates()
				<-monitor.done
			}()
		}

		work := func(ctx context.Context, scanNow <-chan struct{}) error {
			start := func(settings config.Upload, logPath string, logger *slog.Logger) error {
				status.ready(logPath)

				err := runConfigured(ctx, settings, limiter, opts, logger, status, scanNow, updatePending)
				if err != nil && !errors.Is(err, errUpdatePending) {
					err = errors.New(settings.Redact(err.Error()))
					logger.Error("uploader stopped", "error", err)
				}

				return err
			}

			if !needsSetup {
				return start(cfg, logPath, logger)
			}

			if err := announceUpdateReady(); err != nil {
				return err
			}

			settings, ok := waitForSetup(ctx, paths, protector, logPath, logger, status, updatePending)
			if !ok {
				if ctx.Err() == nil {
					return errUpdatePending
				}
				return nil
			}

			if sameDataDir(settings.DataDir, cfg.DataDir) {
				return start(settings, logPath, logger)
			}

			return withData(settings.DataDir, stderr, func(path string, log *slog.Logger) error {
				return start(settings, path, log)
			})
		}

		if opts.Headless || opts.Once {
			return work(uploadCtx, nil)
		}

		return runTray(uploadCtx, stop, status, paths.Config, logPath, logger, checkNow, work)
	})
	if monitor == nil {
		return err
	}
	defer monitor.close()
	if !errors.Is(err, errUpdatePending) {
		return err
	}
	if ctx.Err() != nil {
		return nil
	}

	err = monitor.install(ctx)
	if err == nil {
		return nil
	}
	_ = withData(cfg.DataDir, stderr, func(_ string, logger *slog.Logger) error {
		logger.Error("install update", "error", err)
		return nil
	})
	if _, failed := errors.AsType[*release.RecoveryError](err); failed || ctx.Err() != nil {
		return err
	}

	old := relaunch(monitor.updater.Exe)
	if restartErr := old.Start(); restartErr != nil {
		return errors.Join(err, restartErr)
	}
	return old.Process.Release()
}

func sameDataDir(a, b string) bool {
	if a == b {
		return true
	}

	first, err := os.Stat(a)
	if err != nil {
		return false
	}

	second, err := os.Stat(b)

	return err == nil && os.SameFile(first, second)
}

func waitForSetup(
	ctx context.Context,
	paths config.UploadPaths,
	protector config.SecretProtector,
	logPath string,
	logger *slog.Logger,
	status *Status,
	updatePending <-chan struct{},
) (config.Upload, bool) {
	logger.Info("waiting for setup", "config", paths.Config)

	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()

	lastError := ""
	for {
		select {
		case <-ctx.Done():
			return config.Upload{}, false
		case <-updatePending:
			return config.Upload{}, false
		case <-tick.C:
		}

		cfg, err := config.LoadUpload(paths, protector)
		if err == nil {
			status.setup(cfg.Server, logPath)
			err = cfg.ValidateStorage()
		}

		if err == nil {
			return cfg, ctx.Err() == nil
		}

		if brief := err.Error(); brief != lastError && !errors.Is(err, config.ErrUploadSetup) {
			logger.Warn("read setup config", "error", brief)
		}

		lastError = err.Error()
	}
}

func runConfigured(
	ctx context.Context,
	cfg config.Upload,
	limiter *riot.Limiter,
	opts Options,
	logger *slog.Logger,
	status *Status,
	scanNow <-chan struct{},
	updatePending <-chan struct{},
) error {
	store, err := source.New(ctx, source.Config{
		Endpoint:     cfg.S3Endpoint,
		Region:       cfg.S3Region,
		AccessKey:    cfg.S3AccessKey,
		SecretKey:    cfg.S3SecretKey,
		SessionToken: cfg.S3SessionToken,
		Bucket:       cfg.S3Bucket,
		Prefix:       keyPrefix,
	})
	if err != nil {
		return err
	}

	var connect func(context.Context) (Session, error)
	if runtime.GOOS == "windows" {
		connect = func(ctx context.Context) (Session, error) {
			client, err := riot.Connect(ctx, limiter)
			if err != nil {
				return nil, err
			}

			return client, nil
		}
	}

	app, err := New(cfg, store, connect, logger, status)
	if err != nil {
		return err
	}

	if opts.DemosDir != "" {
		app.demos = opts.DemosDir
	}
	if runtime.GOOS == "windows" {
		app.matchActivity = riot.MatchActivity
	}

	logger.Info("uploader started", "scan_interval", cfg.ScanInterval.String(), "friends", cfg.Friends,
		"friend_interval", cfg.FriendInterval.String())

	if opts.Once {
		return app.run(ctx, nil, nil, true)
	}

	if err := announceUpdateReady(); err != nil {
		return err
	}

	return app.Run(ctx, scanNow, updatePending)
}

func withData(dir string, stderr io.Writer, run func(string, *slog.Logger) error) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	lock := flock.New(filepath.Join(dir, "upload.lock"))

	defer func() {
		_ = lock.Close()
	}()

	locked, err := lock.TryLock()
	if err != nil {
		return err
	}

	if !locked {
		return errors.New("another uploader is using " + dir)
	}

	logPath := filepath.Join(dir, "upload.log")
	logs, err := OpenRotatingLog(logPath)
	if err != nil {
		return err
	}

	defer func() {
		_ = logs.Close()
	}()

	var output io.Writer = logs
	if stderr != nil {
		output = io.MultiWriter(logs, stderr)
	}

	return run(logPath, slog.New(slog.NewTextHandler(output, nil)))
}
