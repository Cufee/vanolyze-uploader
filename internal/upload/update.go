package upload

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Cufee/vanolyze-uploader/internal/config"
	"github.com/Cufee/vanolyze-uploader/internal/release"
	"github.com/gofrs/flock"
)

const updateReadyEnv = "VANOLYZE_UPLOADER_UPDATE_READY"

var errUpdatePending = errors.New("verified update is ready to install")

type updateMonitor struct {
	updater release.Updater
	paths   config.UploadPaths
	check   chan struct{}
	pending chan struct{}
	done    chan struct{}
	staged  string
	build   release.Build
	lock    *flock.Flock
}

func newUpdateMonitor(paths config.UploadPaths, opts Options) *updateMonitor {
	if opts.Once || !release.RunningBuild().Valid() {
		return nil
	}
	key, err := release.EmbeddedKey()
	if err != nil {
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return nil
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return nil
	}
	return &updateMonitor{
		updater: release.NewUpdater(release.GitHub{}, key, release.RunningBuild(), exe),
		paths:   paths, check: make(chan struct{}, 1),
		pending: make(chan struct{}), done: make(chan struct{}),
	}
}

func (m *updateMonitor) run(ctx context.Context, logger *slog.Logger, status *Status) {
	defer close(m.done)

	// Jitter checks across installations, including just after a release restart.
	timer := time.NewTimer(5*time.Second + rand.N(55*time.Second))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.check:
		case <-timer.C:
		}
		var offer release.Offer
		cfg, err := config.LoadUpload(m.paths, config.PlatformSecretProtector())
		if err == nil && cfg.Update.Channel == "stable" {
			offer, err = m.prepare(ctx, cfg.Update.Auto)
		}

		updateText := "Current"
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			logger.Warn("check for updates", "error", err)
			updateText = "Check failed"
		case cfg.Update.Channel != "stable":
			updateText = "Unsupported channel"
		case offer.Build.Valid() && cfg.Update.Auto:
			logger.Info("verified update ready", "version", offer.Version)
			status.update(func(v *Snapshot) { v.Updates = "Restart pending" })
			close(m.pending)
			return
		case offer.Build.Valid():
			logger.Info("update available", "version", offer.Version)
			updateText = offer.Version + " available"
		}
		status.update(func(v *Snapshot) { v.Updates = updateText })
		timer.Reset(6*time.Hour + rand.N(30*time.Minute))
	}
}

func (m *updateMonitor) prepare(ctx context.Context, auto bool) (release.Offer, error) {
	lock := flock.New(m.updater.Exe + ".update-lock")
	locked, err := lock.TryLock()
	if err != nil || !locked {
		_ = lock.Close()
		if err != nil {
			return release.Offer{}, err
		}
		return release.Offer{}, errors.New("an update check is already in progress")
	}

	// Transfer ownership to the watchdog only after successful staging.
	defer func() {
		if m.lock == nil {
			_ = lock.Close()
		}
	}()

	offer, err := m.updater.Check(ctx)
	if err != nil || !offer.Build.Valid() || !auto {
		return offer, err
	}
	staged, err := m.updater.Prepare(ctx, offer, m.probe)
	if err != nil {
		return release.Offer{}, err
	}
	m.staged, m.build, m.lock = staged, offer.Build, lock
	return offer, nil
}

func (m *updateMonitor) close() {
	if m.lock != nil {
		_ = os.Remove(m.staged)
		_ = m.lock.Close()
	}
}

func (m *updateMonitor) probe(ctx context.Context, path string, build release.Build) error {
	if _, err := os.Stat(m.paths.Config); err != nil {
		return fmt.Errorf("read current config: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, path, "-selfcheck", "-config", m.paths.Config)
	hideProbeWindow(cmd)
	output, err := cmd.Output()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return err
	}
	if string(output) != build.String()+"\n" {
		return errors.New("unexpected version or sequence from self-check")
	}
	return nil
}

func (m *updateMonitor) install(ctx context.Context) error {
	ready := m.staged + ".ready"
	defer os.Remove(ready)

	return m.updater.Install(ctx, m.staged, m.build, m.probe, func(ctx context.Context, path string, build release.Build) (func() error, error) {
		return startReplacement(ctx, path, ready, build)
	})
}

func relaunch(exe string) *exec.Cmd {
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd
}

func startReplacement(ctx context.Context, path, ready string, build release.Build) (func() error, error) {
	cmd := relaunch(path)
	cmd.Env = append(cmd.Environ(), updateReadyEnv+"="+ready)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start replacement: %w", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	stop := func() error {
		if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return fmt.Errorf("stop replacement: %w", err)
		}
		<-exited
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case err := <-exited:
			if err != nil {
				return nil, fmt.Errorf("replacement exited before readiness: %w", err)
			}
			return nil, errors.New("replacement exited before readiness")
		case <-ctx.Done():
			cause := fmt.Errorf("wait for replacement startup: %w", ctx.Err())
			if err := stop(); err != nil {
				return nil, &release.RecoveryError{Err: errors.Join(cause, err)}
			}
			return nil, cause
		case <-tick.C:
			if data, err := os.ReadFile(ready); err == nil && string(data) == build.String() {
				select {
				case err := <-exited:
					return nil, fmt.Errorf("replacement stopped during startup (exit error: %v)", err)
				default:
					return stop, nil
				}
			}
		}
	}
}

// announceUpdateReady runs within the child's upload lock after local startup.
func announceUpdateReady() error {
	path := os.Getenv(updateReadyEnv)
	if path == "" {
		return nil
	}
	_ = os.Unsetenv(updateReadyEnv)

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return err
	}
	name := filepath.Base(path)
	if filepath.Join(filepath.Dir(exe), name) != path ||
		!strings.HasPrefix(name, ".vanolyze-update-") || !strings.HasSuffix(name, ".ready") {
		return errors.New("invalid update readiness path")
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = file.WriteString(release.RunningBuild().String())
	return errors.Join(err, file.Close())
}
