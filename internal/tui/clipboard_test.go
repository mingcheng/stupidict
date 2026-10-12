package tui

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	dictionary "github.com/mingcheng/stupidict"
	"github.com/mingcheng/stupidict/internal/clipboard"
	"github.com/mingcheng/stupidict/internal/profile"
	"github.com/rivo/tview"
)

func clipboardIdle(ui *UI) {
	ui.app.SetFocus(ui.results)
	ui.clipboard.lastActivity = time.Time{}
}

func clipboardCopy(ui *UI, word string) {
	now := time.Now()
	ui.clipboardSample(word, now)
	ui.clipboardSample(word, now.Add(clipboardDebounce))
}

func TestClipboardBaselineDebounceAndQuietResult(t *testing.T) {
	var calls atomic.Int32
	h := startUI(t, &engineStub{lookup: func(_ context.Context, word string) (dictionary.LookupResult, error) {
		calls.Add(1)
		return dictionary.LookupResult{Query: word, Primary: &dictionary.Entry{Word: word, Translation: "meaning"}}, nil
	}})
	h.onUI(func() {
		ui := h.ui
		clipboardIdle(ui)
		now := time.Now()
		ui.clipboardSample("old", now)
		ui.clipboardSample("old", now.Add(time.Second))
		ui.clipboardSample("apple", now.Add(2*time.Second))
		ui.clipboardSample("banana", now.Add(2200*time.Millisecond))
		ui.clipboardSample("banana", now.Add(2699*time.Millisecond))
		if calls.Load() != 0 {
			t.Error("baseline or unstable clipboard was queried")
		}
		ui.clipboardSample("banana", now.Add(2700*time.Millisecond))
	})
	h.wait(func() bool { return h.ui.current != nil && h.ui.current.Query == "banana" })
	h.onUI(func() {
		if !h.ui.results.HasFocus() || h.ui.search.GetText() != "banana" {
			t.Error("clipboard changed focus or failed to update the query")
		}
		clipboardCopy(h.ui, "banana")
		clipboardCopy(h.ui, "")
		clipboardCopy(h.ui, "banana")
		if calls.Load() != 1 {
			t.Error("duplicate / current word queried again")
		}
	})
	records, err := h.service.History()
	if err != nil || len(records) != 0 {
		t.Fatalf("clipboard query persisted: %+v %v", records, err)
	}
	h.speaker.gate.Lock()
	defer h.speaker.gate.Unlock()
	if len(h.speaker.words) != 0 {
		t.Fatal("clipboard query played audio")
	}
}

func TestClipboardSkipsBusyContentWithoutReplay(t *testing.T) {
	for _, reason := range []string{"input", "draft", "dialog", "configure", "download", "dictionary", "lookup", "playback", "recent activity", "audio focus"} {
		t.Run(reason, func(t *testing.T) {
			var calls atomic.Int32
			h := startUI(t, &engineStub{lookup: func(_ context.Context, word string) (dictionary.LookupResult, error) {
				calls.Add(1)
				return dictionary.LookupResult{Query: word}, nil
			}})
			h.onUI(func() {
				ui := h.ui
				clipboardIdle(ui)
				ui.clipboardSample("", time.Now())
				switch reason {
				case "input":
					ui.app.SetFocus(ui.search)
				case "draft":
					ui.setSearch("unfinished")
				case "dialog":
					ui.help()
				case "configure":
					ui.configuring = true
				case "download":
					ui.downloading = true
				case "dictionary":
					ui.dictionaryPending = true
				case "lookup":
					ui.queryCancel = func() {}
				case "playback":
					ui.speechCancel = func() {}
				case "recent activity":
					ui.clipboardActivity()
				case "audio focus":
					ui.british.SetDisabled(false)
					ui.app.SetFocus(ui.british)
				}
				clipboardCopy(ui, "ignored")
				if ui.dialog {
					ui.closeDialog()
				}
				ui.configuring, ui.downloading, ui.dictionaryPending = false, false, false
				ui.queryCancel, ui.speechCancel = nil, nil
				ui.setSearch("")
				clipboardIdle(ui)
				clipboardCopy(ui, "ignored")
				if calls.Load() != 0 {
					t.Error("busy clipboard content was queried or replayed")
				}
				clipboardCopy(ui, "next")
			})
			h.wait(func() bool { return calls.Load() == 1 && h.ui.clipboard.queryCancel == nil })
		})
	}
}

func TestClipboardQuietThreshold(t *testing.T) {
	h := startUI(t, &engineStub{})
	h.onUI(func() {
		clipboardIdle(h.ui)
		now := time.Now()
		h.ui.clipboard.lastActivity = now
		if !h.ui.clipboardBusy(now.Add(clipboardQuiet-time.Nanosecond)) ||
			h.ui.clipboardBusy(now.Add(clipboardQuiet)) {
			t.Error("quiet period must be exactly three seconds")
		}
	})
}

func TestClipboardStaleResultsDiscarded(t *testing.T) {
	for _, action := range []string{"typing", "mouse", "manual lookup", "new clipboard", "disable", "dialog", "navigation"} {
		t.Run(action, func(t *testing.T) {
			started, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
			h := startUI(t, &engineStub{lookup: func(_ context.Context, word string) (dictionary.LookupResult, error) {
				if word == "stale" {
					close(started)
					<-release
					close(finished)
				}
				return dictionary.LookupResult{Query: word, Primary: &dictionary.Entry{Word: word}}, nil
			}})
			defer close(release)
			h.query("original")
			h.onUI(func() {
				clipboardIdle(h.ui)
				h.ui.clipboardSample("", time.Now())
				clipboardCopy(h.ui, "stale")
			})
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("clipboard lookup not started")
			}
			h.onUI(func() {
				ui := h.ui
				switch action {
				case "typing":
					ui.search.SetText("draft")
				case "mouse":
					ui.app.GetMouseCapture()(tcell.NewEventMouse(0, 0, tcell.WheelDown, tcell.ModNone), tview.MouseScrollDown)
				case "manual lookup":
					ui.lookup("manual")
				case "new clipboard":
					ui.clipboardSample("", time.Now())
				case "disable":
					ui.config.MonitorClipboard = false
					ui.startClipboard()
				case "dialog":
					ui.help()
				case "navigation":
					ui.move(-1)
				}
			})
			// Release through a separate channel send so the deferred close also
			// unblocks the stub if an earlier assertion fails.
			release <- struct{}{}
			<-finished
			h.wait(func() bool { return h.ui.clipboard.queryCancel == nil })
			h.onUI(func() {
				if h.ui.current.Query == "stale" {
					t.Error("stale clipboard result replaced user state")
				}
			})
			records, err := h.service.History()
			if err != nil {
				t.Fatal(err)
			}
			for _, record := range records {
				if record.Query == "stale" {
					t.Error("stale clipboard result persisted")
				}
			}
		})
	}
}

func TestClipboardMissAndErrorKeepCurrentPage(t *testing.T) {
	h := startUI(t, &engineStub{lookup: func(_ context.Context, word string) (dictionary.LookupResult, error) {
		if word == "missing" {
			return dictionary.LookupResult{Query: word}, nil
		}
		if word == "failure" {
			return dictionary.LookupResult{}, errors.New("lookup unavailable")
		}
		return dictionary.LookupResult{Query: word, Primary: &dictionary.Entry{Word: word}}, nil
	}})
	h.query("original")
	h.onUI(func() { clipboardIdle(h.ui); h.ui.clipboardSample("", time.Now()); clipboardCopy(h.ui, "missing") })
	h.wait(func() bool { return h.ui.clipboard.queryCancel == nil })
	h.onUI(func() {
		if h.ui.current.Query != "original" {
			t.Error("miss replaced useful content")
		}
		clipboardCopy(h.ui, "failure")
	})
	h.wait(func() bool { return strings.Contains(h.ui.status.GetText(true), "lookup unavailable") })
	h.onUI(func() {
		if h.ui.current.Query != "original" {
			t.Error("error replaced useful content")
		}
	})
}

func TestClipboardSettingsLifecycle(t *testing.T) {
	started, stopped := make(chan struct{}, 2), make(chan struct{}, 2)
	var opens atomic.Int32
	h := startUI(t, &engineStub{}, func(ui *UI) {
		ui.clipboard.open = func() (clipboard.Reader, error) {
			opens.Add(1)
			return func(ctx context.Context) (string, error) {
				started <- struct{}{}
				<-ctx.Done()
				stopped <- struct{}{}
				return "", ctx.Err()
			}, nil
		}
	})
	if opens.Load() != 0 {
		t.Fatal("default-off monitor accessed clipboard")
	}
	save := func(enabled bool) {
		h.key(tcell.KeyF3, 0)
		h.wait(func() bool { return h.ui.dialog })
		h.onUI(func() {
			form := h.ui.dialogBody.(*tview.Form)
			form.GetFormItem(5).(*tview.Checkbox).SetChecked(enabled)
			h.ui.app.SetFocus(form.GetButton(0))
		})
		h.key(tcell.KeyEnter, 0)
		h.wait(func() bool { return !h.ui.dialog && !h.ui.configuring && h.ui.config.MonitorClipboard == enabled })
		config, err := profile.Load(h.ui.configPath)
		if err != nil || config.MonitorClipboard != enabled {
			t.Fatalf("clipboard setting not persisted: %+v %v", config, err)
		}
	}
	await := func(channel <-chan struct{}) {
		t.Helper()
		select {
		case <-channel:
		case <-time.After(3 * time.Second):
			t.Fatal("monitor lifecycle timed out")
		}
	}
	save(true)
	await(started)
	save(false)
	await(stopped)
	save(true)
	await(started)
	h.onUI(h.ui.cancel)
	await(stopped)
}

func TestClipboardMonitorFailureAndRetry(t *testing.T) {
	var attempts atomic.Int32
	h := startUI(t, &engineStub{}, func(ui *UI) {
		ui.config.MonitorClipboard = true
		ui.clipboard.open = func() (clipboard.Reader, error) {
			attempts.Add(1)
			return nil, errors.New("missing clipboard helper")
		}
		ui.build()
	})
	h.wait(func() bool { return h.ui.clipboard.failed })
	h.onUI(func() {
		if !strings.Contains(h.ui.status.GetText(true), "missing clipboard helper") {
			t.Error("monitor failure not surfaced")
		}
		h.ui.startClipboard()
	})
	h.wait(func() bool { return attempts.Load() == 2 && h.ui.clipboard.failed })
}

func TestClipboardPollingAndReadFailure(t *testing.T) {
	var text atomic.Value
	text.Store("initial")
	var fail atomic.Bool
	h := startUI(t, &engineStub{}, func(ui *UI) {
		ui.config.MonitorClipboard = true
		ui.clipboard.open = func() (clipboard.Reader, error) {
			return func(context.Context) (string, error) {
				if fail.Load() {
					return "", errors.New("clipboard read denied")
				}
				return text.Load().(string), nil
			}, nil
		}
		ui.build()
	})
	h.wait(func() bool { return h.ui.clipboard.initialized })
	h.onUI(func() {
		if h.ui.current != nil {
			t.Error("startup clipboard was queried")
		}
		clipboardIdle(h.ui)
	})
	text.Store(" Apple \n")
	h.wait(func() bool { return h.ui.current != nil && h.ui.current.Query == "apple" })
	fail.Store(true)
	h.wait(func() bool { return h.ui.clipboard.failed })
	h.onUI(func() {
		if !strings.Contains(h.ui.status.GetText(true), "clipboard read denied") {
			t.Error("read error not surfaced")
		}
		h.ui.renderClipboardStatus()
		if !strings.Contains(h.ui.clipboardStatus.GetText(true), "error") {
			t.Error("error indicator not persistent")
		}
	})
}
