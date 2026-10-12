package tui

import (
	"context"
	"strings"
	"time"

	"github.com/mingcheng/stupidict/internal/clipboard"
)

const (
	clipboardPoll     = 750 * time.Millisecond
	clipboardDebounce = 500 * time.Millisecond
	clipboardQuiet    = 3 * time.Second
)

type clipboardMonitor struct {
	open                        func() (clipboard.Reader, error)
	cancel, queryCancel         context.CancelFunc
	generation, queryGeneration uint64
	initialized, failed         bool
	last, pending               string
	changed, lastActivity       time.Time
}

func (ui *UI) startClipboard() {
	monitor := &ui.clipboard
	if monitor.cancel != nil {
		monitor.cancel()
		monitor.cancel = nil
	}
	ui.cancelClipboardQuery()
	monitor.generation++
	monitor.initialized, monitor.failed = false, false
	monitor.last, monitor.pending = "", ""
	if !ui.config.MonitorClipboard {
		return
	}
	ctx, cancel := context.WithCancel(ui.ctx)
	monitor.cancel = cancel
	generation, open := monitor.generation, monitor.open
	ui.work(ctx, func(ctx context.Context) func() {
		reader, err := open()
		if err == nil {
			ticker := time.NewTicker(clipboardPoll)
			defer ticker.Stop()
			for {
				var text string
				observed := time.Now()
				text, err = reader(ctx)
				if err != nil || ctx.Err() != nil {
					break
				}
				word := clipboard.Word(text)
				select {
				case ui.updates <- func() {
					if generation == ui.clipboard.generation && ctx.Err() == nil {
						ui.clipboardSample(word, observed)
					}
				}:
				case <-ctx.Done():
					return nil
				}
				select {
				case <-ticker.C:
				case <-ctx.Done():
					return nil
				}
			}
		}
		if ctx.Err() != nil {
			return nil
		}
		return func() {
			if generation != ui.clipboard.generation {
				return
			}
			ui.cancelClipboardQuery()
			monitor.failed = true
			ui.status.SetText(ui.tr("Clipboard monitoring stopped; save Settings to retry: ", "剪贴板监控已停止；保存设置可重试：") + clean(err.Error()))
		}
	})
}

func (ui *UI) cancelClipboardQuery() {
	ui.clipboard.queryGeneration++
	if ui.clipboard.queryCancel != nil {
		ui.clipboard.queryCancel()
		ui.clipboard.queryCancel = nil
	}
}

func (ui *UI) clipboardActivity() {
	ui.clipboard.lastActivity = time.Now()
	ui.clipboard.pending = ""
	ui.cancelClipboardQuery()
}

func (ui *UI) clipboardBusy(now time.Time) bool {
	// Preserve unfinished text even after focus leaves the input field.
	draft := ui.search.GetText() != "" && (ui.current == nil || ui.search.GetText() != ui.current.Query)
	return ui.dialog || ui.configuring || ui.downloading || ui.dictionaryPending ||
		ui.search.HasFocus() || draft || ui.british.HasFocus() || ui.american.HasFocus() ||
		ui.queryCancel != nil || ui.speechCancel != nil ||
		now.Sub(ui.clipboard.lastActivity) < clipboardQuiet
}

func (ui *UI) clipboardSample(word string, observed time.Time) {
	monitor := &ui.clipboard
	if !monitor.initialized {
		monitor.initialized, monitor.last = true, word
		return
	}
	if word != monitor.last {
		ui.cancelClipboardQuery()
		monitor.last, monitor.pending, monitor.changed = word, "", observed
		if !ui.clipboardBusy(observed) {
			monitor.pending = word
		}
		return
	}
	if monitor.pending == "" {
		return
	}
	if ui.clipboardBusy(observed) {
		monitor.pending = ""
		return
	}
	if observed.Sub(monitor.changed) < clipboardDebounce {
		return
	}
	monitor.pending = ""
	if ui.current != nil && (strings.EqualFold(word, ui.current.Query) ||
		ui.current.Primary != nil && strings.EqualFold(word, ui.current.Primary.Word)) {
		return
	}
	ui.lookupClipboard(word)
}

func (ui *UI) lookupClipboard(word string) {
	ui.cancelClipboardQuery()
	ctx, cancel := context.WithTimeout(ui.ctx, 15*time.Second)
	ui.clipboard.queryCancel = cancel
	generation := ui.clipboard.queryGeneration
	ui.work(ctx, func(ctx context.Context) func() {
		defer cancel()
		result, err := ui.backend.Lookup(ctx, word)
		return func() {
			if generation != ui.clipboard.queryGeneration {
				return
			}
			ui.clipboard.queryCancel = nil
			if ui.clipboardBusy(time.Now()) {
				return
			}
			if err != nil {
				ui.fail(err)
				return
			}
			// A random token or a fuzzy-only match must not replace a useful page.
			if result.Primary == nil {
				return
			}
			ui.invalidate()
			ui.navigation.Push(result)
			ui.setSearch(result.Query)
			ui.render(result)
			ui.status.SetText(ui.tr("Clipboard: ", "剪贴板：") + clean(ui.summary(result)))
		}
	})
}

func (ui *UI) renderClipboardStatus() {
	if !ui.config.MonitorClipboard {
		return
	}
	text := ui.tr("Clipboard: on", "剪贴板：已开启")
	if ui.clipboard.failed {
		text = ui.tr("Clipboard: error (F3)", "剪贴板：异常 (F3)")
	} else if ui.clipboardBusy(time.Now()) {
		text = ui.tr("Clipboard: paused", "剪贴板：暂停中")
	}
	ui.clipboardStatus.SetText(text)
}
