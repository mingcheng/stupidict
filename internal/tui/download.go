package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/mingcheng/stupidict/internal/assets"
)

// SetInitializer connects bootstrapping without putting network calls in widgets.
// Call before Run; tests can supply a deterministic, cancellation-aware worker.
func (ui *UI) SetInitializer(initialize func(context.Context, bool, assets.Reporter) error, force, ready bool) *UI {
	ui.initializer, ui.initialForce, ui.dictionaryPending = initialize, force, !ready
	return ui
}

func (ui *UI) toggleDownload() {
	if ui.downloading {
		ui.downloadCancel()
		return
	}
	if ui.downloadError != nil {
		ui.initialize(ui.downloadForce)
	}
}

func (ui *UI) initialize(force bool) {
	if ui.initializer == nil || ui.downloading || ui.configuring {
		return
	}
	ui.downloadForce = force
	ui.downloading = true
	ui.downloadError = nil
	ui.downloadProgress = assets.Progress{Phase: "prepare"}
	ctx, cancel := context.WithCancel(ui.ctx)
	ui.downloadCancel = cancel
	ui.renderDownload()
	ui.work(ctx, func(ctx context.Context) func() {
		defer cancel()
		report := func(progress assets.Progress) {
			update := func() {
				ui.downloadProgress = progress
				if progress.Asset == assets.Dictionary && progress.Phase == "ready" {
					ui.dictionaryPending = false
				}
				if progress.Asset == assets.Audio && progress.Phase == "ready" {
					ui.refreshPronunciation()
				}
				ui.renderDownload()
			}
			// Data progress is coalescible, but readiness must never be dropped.
			if progress.Phase == "ready" {
				select {
				case ui.updates <- update:
				case <-ui.ctx.Done():
				}
			} else {
				select {
				case ui.updates <- update:
				default:
				}
			}
		}
		err := ui.initializer(ctx, force, report)
		return func() {
			ui.downloading = false
			ui.downloadError = err
			if err == nil {
				ui.downloadForce = false
			}
			ui.renderDownload()
		}
	})
}

func (ui *UI) renderDownload() {
	if ui.downloadInfo == nil {
		return
	}
	ui.search.SetDisabled(ui.dictionaryPending)
	ui.downloadButton.SetDisabled(ui.initializer == nil)
	ui.footer.Clear()
	if ui.downloading || ui.downloadError != nil {
		ui.root.SetRows(1, 1, 3, 0, 3)
		ui.footer.AddItem(ui.downloadPane, 2, 0, false)
		ui.downloadButton.SetLabel(ui.iconLabel("\uf04d", ui.tr("Cancel", "取消")))
		progress := ui.downloadProgress
		phase := progress.Phase
		switch phase {
		case "prepare":
			phase = ui.tr("Preparing data", "准备数据")
		case "metadata":
			phase = ui.tr("Fetching checksum", "获取校验信息")
		case "download":
			phase = ui.tr("Downloading", "下载中")
		case "verify":
			phase = ui.tr("Checking SHA-256", "校验 SHA-256")
		case "extract":
			phase = ui.tr("Extracting", "解压中")
		case "install":
			phase = ui.tr("Installing", "安装中")
		case "validate":
			phase = ui.tr("Checking database", "校验数据库")
		case "ready":
			phase = ui.tr("Ready", "已就绪")
		}
		bar := "[------------]"
		if progress.Total > 0 {
			filled := min(12, int(float64(progress.Received)/float64(progress.Total)*12))
			bar = "[" + strings.Repeat("#", filled) + strings.Repeat("-", 12-filled) + "]"
			bar += fmt.Sprintf(" %3.0f%%", float64(progress.Received)/float64(progress.Total)*100)
		}
		resume := ""
		if progress.Resumed > 0 {
			resume = ui.tr(" (resuming)", " (断点续传)")
		}
		line := fmt.Sprintf("%s %s %s%s", bar, progress.Asset, phase, resume)
		detail := progress.Source
		if progress.Received > 0 {
			detail = fmt.Sprintf("%.1f MiB  %s", float64(progress.Received)/(1024*1024), detail)
		}
		if ui.downloadError != nil {
			ui.downloadButton.SetLabel(ui.iconLabel("\uf01e", ui.tr("Retry", "重试")))
			line = ui.tr("Data error: ", "数据错误：") + clean(ui.downloadError.Error())
			if errors.Is(ui.downloadError, context.Canceled) {
				line = ui.tr("Download paused", "下载已暂停")
			}
			detail = assets.ReleasePage
		}
		ui.downloadInfo.SetText(line + "\n" + detail)
	} else {
		ui.root.SetRows(1, 1, 3, 0, 1)
		if ui.downloadButton.HasFocus() {
			ui.app.SetFocus(ui.search)
		}
	}
	if ui.config.MonitorClipboard {
		ui.footer.AddItem(ui.statusLine, 1, 0, false)
	} else {
		ui.footer.AddItem(ui.status, 1, 0, false)
	}
}
