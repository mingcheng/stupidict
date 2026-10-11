package main

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/mingcheng/stupidict/internal/assets"
	"github.com/mingcheng/stupidict/internal/client"
	"github.com/mingcheng/stupidict/internal/profile"
)

func TestBuildInfo(t *testing.T) {
	previousVersion, previousCommit, previousBuildDate := version, commit, buildDate
	t.Cleanup(func() {
		version, commit, buildDate = previousVersion, previousCommit, previousBuildDate
	})
	version, commit, buildDate = "v1.2.3", "abc1234", "2026-10-11T12:00:00Z"
	info := buildInfo()
	for _, expected := range []string{
		"v1.2.3", "Commit: abc1234", "Built: 2026-10-11T12:00:00Z",
		"Go: " + runtime.Version(), "Platform: " + runtime.GOOS + "/" + runtime.GOARCH,
	} {
		if !strings.Contains(info, expected) {
			t.Errorf("build info missing %q: %s", expected, info)
		}
	}
}

func TestStartupFlagsDoNotLaunchOrTouchProfile(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("HOME", directory)
	t.Setenv("USERPROFILE", directory)
	launch := func(context.Context, *client.Service, string, bool) error {
		t.Fatal("unexpected UI launch")
		return nil
	}
	for _, arguments := range [][]string{{"-h"}, {"-version"}} {
		if err := runWithUI(context.Background(), arguments, launch); err != nil {
			t.Fatal(err)
		}
	}
	for _, arguments := range [][]string{{"-unknown"}, {"unexpected"}} {
		if err := runWithUI(context.Background(), arguments, launch); err == nil {
			t.Fatal("invalid arguments accepted")
		}
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 0 {
		t.Fatalf("flags touched profile: %v %v", entries, err)
	}
}

func TestStartupPersistsPathsAndRejectsBrokenOverrides(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "config.json")
	dictionaryPath := filepath.Join(directory, "dictionary.db")
	database, err := sql.Open("sqlite", dictionaryPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = database.Exec(`CREATE TABLE word(word TEXT, phonetic TEXT, pos TEXT, translation TEXT, definition TEXT, freq INTEGER, bnc INTEGER, tag TEXT)`)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	launched := false
	launch := func(ctx context.Context, service *client.Service, actualPath string, _ bool) error {
		launched = true
		if actualPath != path || service.Config().Dictionary != dictionaryPath {
			t.Error("startup paths differ")
		}
		config, err := profile.Load(path)
		if err != nil || config.Dictionary != dictionaryPath {
			t.Errorf("configuration not persisted: %+v %v", config, err)
		}
		return nil
	}
	if err := runWithUI(context.Background(), []string{"-config", path, "-dict", dictionaryPath}, launch); err != nil {
		t.Fatal(err)
	}
	if !launched {
		t.Fatal("UI not launched")
	}
	before, _ := os.ReadFile(path)
	launch = func(context.Context, *client.Service, string, bool) error {
		t.Fatal("invalid asset launched UI")
		return nil
	}
	if err := runWithUI(context.Background(), []string{"-config", path, "-dict", filepath.Join(directory, "missing.db")}, launch); err == nil {
		t.Fatal("invalid dictionary accepted")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("bad override replaced saved config")
	}
	if err := os.WriteFile(path, []byte("broken json"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runWithUI(context.Background(), []string{"-config", path}, launch); err == nil {
		t.Fatal("malformed config ignored")
	}
}

func TestDownloadAndUpdateFlags(t *testing.T) {
	for _, test := range []struct {
		name            string
		arguments       []string
		headless, force bool
	}{
		{"default", nil, false, false},
		{"download", []string{"-download"}, true, false},
		{"update-ui", []string{"-update-data"}, false, true},
		{"update-headless", []string{"-download", "-update-data"}, true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			arguments := append([]string{"-config", path}, test.arguments...)
			launched, initialized := false, false
			launch := func(_ context.Context, _ *client.Service, _ string, force bool) error {
				launched = true
				if force != test.force {
					t.Error("UI force flag lost")
				}
				return nil
			}
			initialize := func(_ context.Context, service *client.Service, force bool, _ assets.Reporter) error {
				initialized = true
				if force != test.force {
					t.Error("download force flag lost")
				}
				if service.Config().Dictionary != filepath.Join(filepath.Dir(path), "dictionary.db") {
					t.Error("default data directory differs")
				}
				return nil
			}
			if err := runWithActions(context.Background(), arguments, launch, initialize); err != nil {
				t.Fatal(err)
			}
			if initialized != test.headless || launched == test.headless {
				t.Fatalf("launched=%t initialized=%t", launched, initialized)
			}
		})
	}
	failed := errors.New("offline")
	err := runWithActions(context.Background(), []string{"-config", filepath.Join(t.TempDir(), "config.json"), "-download"}, nil, func(context.Context, *client.Service, bool, assets.Reporter) error { return failed })
	if !errors.Is(err, failed) {
		t.Fatalf("download error lost: %v", err)
	}
}
