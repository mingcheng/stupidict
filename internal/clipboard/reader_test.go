package clipboard

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestClipboardHelperProcess(t *testing.T) {
	if len(os.Args) < 3 || os.Args[len(os.Args)-2] != "--clipboard-test-helper" {
		return
	}
	switch os.Args[len(os.Args)-1] {
	case "text":
		fmt.Print(" Apple \n")
	case "oversized":
		fmt.Print(strings.Repeat("a", maxText*2))
	case "failure":
		fmt.Fprint(os.Stderr, "private clipboard content")
		os.Exit(1)
	case "blocked":
		time.Sleep(time.Minute)
	}
	os.Exit(0)
}

func TestReaderBoundedErrorsAndCancellation(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"text", "oversized", "failure", "blocked"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "blocked" {
				ctx, cancel = context.WithTimeout(ctx, 100*time.Millisecond)
				defer cancel()
			}
			start := time.Now()
			text, err := read(ctx, executable, []string{"-test.run=^TestClipboardHelperProcess$", "--", "--clipboard-test-helper", mode})
			switch mode {
			case "text":
				if err != nil || Word(text) != "apple" {
					t.Fatalf("text read = %q, %v", text, err)
				}
			case "oversized":
				if err != nil || len(text) != maxText+1 || Word(text) != "" {
					t.Fatalf("oversized read length = %d, %v", len(text), err)
				}
			case "failure":
				if err == nil || strings.Contains(err.Error(), "private") || text != "" {
					t.Fatalf("error leaked data or was hidden: %q, %v", text, err)
				}
			case "blocked":
				if err == nil || time.Since(start) > time.Second {
					t.Fatalf("reader did not cancel promptly: %v", err)
				}
			}
		})
	}
}

func TestWord(t *testing.T) {
	for _, test := range []struct{ text, want string }{
		{" Apple \r\n", "apple"}, {"I", "i"}, {"don't", "don't"}, {"well-known", "well-known"},
		{"", ""}, {" \n", ""}, {"two words", ""}, {"hello\nworld", ""}, {"中文", ""},
		{"hello!", ""}, {"https://example.com", ""}, {"user@example.com", ""},
		{"word123", ""}, {"-word", ""}, {"word'", ""}, {"a--b", ""}, {"a-'b", ""},
		{"a\x00b", ""}, {"foo_bar", ""}, {"don’t", ""}, {"café", ""},
		{strings.Repeat("a", 64), strings.Repeat("a", 64)},
		{strings.Repeat("a", 65), ""}, {strings.Repeat(" ", maxText) + "cat", ""},
	} {
		if got := Word(test.text); got != test.want {
			t.Errorf("Word(%q) = %q, want %q", test.text, got, test.want)
		}
	}
}

func TestBoundedTextDoesNotAcceptTruncatedContent(t *testing.T) {
	output := &boundedText{}
	for _, text := range []string{"cat", strings.Repeat(" ", maxText), "more"} {
		n, err := output.Write([]byte(text))
		if n != len(text) || err != nil {
			t.Fatalf("Write = %d, %v", n, err)
		}
	}
	if len(output.text) != maxText+1 || Word(output.text) != "" {
		t.Fatal("oversized clipboard accepted or retained")
	}
}

func TestDesktopCommands(t *testing.T) {
	for _, test := range []struct {
		name, platform, wayland, display, installed, want string
		args                                              []string
	}{
		{"macOS", "darwin", "", "", "pbpaste", "pbpaste", []string{"-Prefer", "txt"}},
		{"Wayland", "linux", "wayland-0", ":0", "wl-paste", "wl-paste", []string{"--no-newline"}},
		{"X11", "linux", "", ":0", "xclip", "xclip", []string{"-selection", "clipboard", "-out"}},
		{"X11 fallback", "linux", "", ":0", "xsel", "xsel", []string{"--clipboard", "--output"}},
		{"Windows", "windows", "", "", "powershell.exe", "powershell.exe", nil},
		{"headless", "linux", "", "", "xclip", "", nil},
		{"missing helper", "darwin", "", "", "", "", nil},
		{"no Wayland fallback", "linux", "wayland-0", ":0", "xclip", "", nil},
		{"unsupported", "plan9", "", "", "", "", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			getenv := func(name string) string {
				if name == "WAYLAND_DISPLAY" {
					return test.wayland
				}
				return test.display
			}
			lookPath := func(name string) (string, error) {
				if name == test.installed {
					return name, nil
				}
				return "", errors.New("not installed")
			}
			name, args, err := command(test.platform, getenv, lookPath)
			if test.want == "" {
				if err == nil {
					t.Fatal("expected explicit unsupported / missing helper error")
				}
				return
			}
			if err != nil || name != test.want || test.args != nil && !reflect.DeepEqual(args, test.args) {
				t.Fatalf("command = %q %q, %v", name, args, err)
			}
		})
	}
}
