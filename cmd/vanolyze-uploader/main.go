package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/Cufee/vanolyze-uploader/internal/release"
	"github.com/Cufee/vanolyze-uploader/internal/upload"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1:]); err != nil {
		logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
		logger.Error("uploader stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	// Scheduled tasks from the previous build may still pass this subcommand.
	if len(args) > 0 && args[0] == "upload" {
		args = args[1:]
	}

	var opts upload.Options
	var usage bytes.Buffer

	flags := flag.NewFlagSet("vanolyze-uploader", flag.ContinueOnError)
	flags.SetOutput(&usage)
	flags.StringVar(&opts.ConfigPath, "config", "", "path to the uploader's JSON config")
	flags.StringVar(&opts.DemosDir, "demos-dir", "", "directory containing local replays")
	flags.BoolVar(&opts.Once, "once", false, "scan once and exit")
	flags.BoolVar(&opts.Probe, "probe", false, "preview the Riot scan without accessing the bucket")
	flags.BoolVar(&opts.Headless, "headless", runtime.GOOS != "windows", "run without the tray icon")
	flags.BoolVar(&opts.SelfCheck, "selfcheck", false, "check the config without contacting Riot or the bucket")
	version := flags.Bool("version", false, "print the version and release sequence")
	releaseKey := flags.Bool("release-key", false, "print the embedded minisign public key")

	err := flags.Parse(args)
	if err != nil || flags.NArg() != 0 || opts.Probe || opts.Once || opts.Headless || opts.SelfCheck || *version || *releaseKey {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		defer cancel()

		removeHandler := attachConsole(cancel)
		defer removeHandler()
	}

	if err != nil {
		_, _ = usage.WriteTo(os.Stderr)
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}

		return err
	}

	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}

	if *releaseKey {
		if _, err := release.EmbeddedKey(); err != nil {
			return err
		}
		_, err := fmt.Fprintln(os.Stdout, release.PublicKey)
		return err
	}

	if !*version {
		opts.Output, opts.Errors = os.Stdout, os.Stderr
		if err := upload.Run(ctx, opts); err != nil {
			return err
		}
	}

	if *version || opts.SelfCheck {
		_, err = fmt.Fprintf(os.Stdout, "vanolyze-uploader %s (sequence %s)\n", release.Version, release.Sequence)
	}

	return err
}
