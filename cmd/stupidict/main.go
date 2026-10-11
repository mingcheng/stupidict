package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	dictionary "github.com/mingcheng/stupidict"
	"github.com/mingcheng/stupidict/internal/assets"
	"github.com/mingcheng/stupidict/internal/client"
	"github.com/mingcheng/stupidict/internal/profile"
	"github.com/mingcheng/stupidict/internal/speech"
	"github.com/mingcheng/stupidict/internal/tui"
)

var (
	version   = "dev"
	commit    = "unknown"
	buildDate = "unknown"
)

func buildInfo() string {
	return fmt.Sprintf("%s\nCommit: %s\nBuilt: %s\nGo: %s\nPlatform: %s/%s",
		version, commit, buildDate, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, arguments []string) error {
	return runWithUI(ctx, arguments, func(ctx context.Context, service *client.Service, path string, force bool) error {
		return tui.New(service, speech.New(), path, buildInfo()).SetInitializer(func(ctx context.Context, force bool, report assets.Reporter) error {
			return service.Initialize(ctx, assets.NewDownloader(), force, report)
		}, force, service.Ready()).Run(ctx)
	})
}

type launchUI func(context.Context, *client.Service, string, bool) error
type initializeAssets func(context.Context, *client.Service, bool, assets.Reporter) error

func runWithUI(ctx context.Context, arguments []string, launch launchUI) error {
	return runWithActions(ctx, arguments, launch, func(ctx context.Context, service *client.Service, force bool, report assets.Reporter) error {
		return service.Initialize(ctx, assets.NewDownloader(), force, report)
	})
}

func runWithActions(ctx context.Context, arguments []string, launch launchUI, initialize initializeAssets) error {
	defaultPath, err := profile.DefaultPath()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("stupidict", flag.ContinueOnError)
	path := flags.String("config", defaultPath, "client JSON configuration path")
	dictPath := flags.String("dict", "", "dictionary path (persisted; relative to working directory)")
	audioPath := flags.String("audio", "", "audio database path (persisted)")
	showVersion := flags.Bool("version", false, "print client version")
	downloadOnly := flags.Bool("download", false, "download missing data and exit (no TUI)")
	force := flags.Bool("update-data", false, "replace installed data from the pinned data release; combine with -download for no TUI")
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *showVersion {
		fmt.Println("stupidict", buildInfo())
		return nil
	}
	if flags.NArg() != 0 {
		return errors.New("usage: stupidict [-config path] [-dict path] [-audio path] [-download] [-update-data]")
	}
	*path, err = filepath.Abs(*path)
	if err != nil {
		return err
	}
	config, err := profile.Load(*path)
	if err != nil {
		return fmt.Errorf("configuration %s: %w", *path, err)
	}
	for _, override := range []struct {
		source string
		target *string
	}{{*dictPath, &config.Dictionary}, {*audioPath, &config.Audio}} {
		if override.source != "" {
			*override.target, err = filepath.Abs(override.source)
			if err != nil {
				return err
			}
		}
	}
	service, err := client.Open(ctx, *path, config, nil)
	if err != nil {
		return err
	}
	defer service.Close()
	// Explicit overrides must be valid before replacing the saved settings.
	// Missing default assets still allow entering the UI and repairing paths.
	if *dictPath != "" && !*downloadOnly && !*force {
		if _, err := service.Lookup(ctx, ""); err != nil {
			return err
		}
	}
	if *audioPath != "" && !*downloadOnly && !*force {
		if _, err := service.Audio(ctx, "", dictionary.American); err != nil && !errors.Is(err, dictionary.ErrAudioNotFound) {
			return err
		}
	}
	if err := profile.Save(*path, config); err != nil {
		return err
	}
	if *downloadOnly {
		lastPhase := ""
		lastPrint := time.Time{}
		report := func(progress assets.Progress) {
			phase := progress.Asset + " " + progress.Phase
			if phase == lastPhase && time.Since(lastPrint) < time.Second && progress.Received != progress.Total {
				return
			}
			lastPhase, lastPrint = phase, time.Now()
			if progress.Total > 0 {
				fmt.Fprintf(os.Stderr, "%s %.1f%% (%.1f MiB)\n", phase, float64(progress.Received)*100/float64(progress.Total), float64(progress.Received)/(1024*1024))
			} else {
				fmt.Fprintln(os.Stderr, phase)
			}
		}
		return initialize(ctx, service, *force, report)
	}
	return launch(ctx, service, *path, *force)
}
