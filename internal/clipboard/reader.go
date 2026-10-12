// Package clipboard reads the desktop clipboard without modifying it.
package clipboard

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

type Reader func(context.Context) (string, error)

const maxText = 4096

// Open resolves a desktop helper once. No command runs until the reader is used.
func Open() (Reader, error) {
	name, args, err := command(runtime.GOOS, os.Getenv, exec.LookPath)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context) (string, error) {
		return read(ctx, name, args)
	}, nil
}

func read(ctx context.Context, name string, args []string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = 100 * time.Millisecond
	output := &boundedText{}
	cmd.Stdout = output
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("clipboard helper failed: %w", err)
	}
	return output.text, nil
}

func command(platform string, getenv func(string) string, lookPath func(string) (string, error)) (string, []string, error) {
	var names []string
	var args [][]string
	switch platform {
	case "darwin":
		names, args = []string{"pbpaste"}, [][]string{{"-Prefer", "txt"}}
	case "windows":
		names = []string{"powershell.exe"}
		args = [][]string{{"-NoProfile", "-NonInteractive", "-STA", "-Command",
			"$ErrorActionPreference = 'Stop'; [Console]::OutputEncoding = [System.Text.UTF8Encoding]::new($false); Add-Type -AssemblyName System.Windows.Forms; [Console]::Write([System.Windows.Forms.Clipboard]::GetText())"}}
	case "linux":
		if getenv("WAYLAND_DISPLAY") != "" {
			names, args = []string{"wl-paste"}, [][]string{{"--no-newline"}}
		} else if getenv("DISPLAY") != "" {
			names = []string{"xclip", "xsel"}
			args = [][]string{{"-selection", "clipboard", "-out"}, {"--clipboard", "--output"}}
		} else {
			return "", nil, errors.New("no desktop clipboard session (DISPLAY or WAYLAND_DISPLAY)")
		}
	default:
		return "", nil, fmt.Errorf("clipboard monitoring is unsupported on %s", platform)
	}
	for index, name := range names {
		if path, err := lookPath(name); err == nil {
			return path, args[index], nil
		}
	}
	return "", nil, fmt.Errorf("clipboard monitoring requires %s", strings.Join(names, " or "))
}

// Keep one overflow byte so oversized data cannot become a valid truncated word.
type boundedText struct{ text string }

func (output *boundedText) Write(data []byte) (int, error) {
	n := len(data)
	remaining := maxText + 1 - len(output.text)
	output.text += string(data[:min(n, remaining)])
	return n, nil
}

// Word accepts a single ASCII English word, including internal apostrophes and
// hyphens. Clipboard sentences, URLs, numbers and other content are not queries.
func Word(text string) string {
	if len(text) > maxText {
		return ""
	}
	word := strings.TrimSpace(text)
	if len(word) == 0 || len(word) > 64 {
		return ""
	}
	letter := func(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
	for index := range len(word) {
		c := word[index]
		if letter(c) {
			continue
		}
		if (c != '\'' && c != '-') || index == 0 || index == len(word)-1 ||
			!letter(word[index-1]) || !letter(word[index+1]) {
			return ""
		}
	}
	return strings.ToLower(word)
}
