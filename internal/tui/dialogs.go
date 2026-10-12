package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/mingcheng/stupidict/internal/client"
	"github.com/mingcheng/stupidict/internal/profile"
	"github.com/rivo/tview"
)

type dialogLayer struct{ *tview.Grid }

const projectURL = "https://github.com/mingcheng/stupidict"

func openBrowserURL(ctx context.Context, url string) error {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.CommandContext(ctx, "open", url)
	case "windows":
		command = exec.CommandContext(ctx, "rundll32", "url.dll,FileProtocolHandler", url)
	default:
		command = exec.CommandContext(ctx, "xdg-open", url)
	}
	command.WaitDelay = time.Second
	return command.Run()
}

// Consume even unhandled clicks so empty dialog space cannot activate the page
// underneath. The embedded grid still routes clicks and scrolling to controls.
func (layer *dialogLayer) MouseHandler() func(tview.MouseAction, *tcell.EventMouse, func(tview.Primitive)) (bool, tview.Primitive) {
	handler := layer.Grid.MouseHandler()
	return func(action tview.MouseAction, event *tcell.EventMouse, focus func(tview.Primitive)) (bool, tview.Primitive) {
		_, capture := handler(action, event, focus)
		return true, capture
	}
}

func (ui *UI) openDialog(content tview.Primitive, width, height int) {
	if ui.dialog {
		return
	}
	ui.dialogFocus = ui.app.GetFocus()
	ui.dialog = true
	ui.dialogBody = content
	frame := tview.NewGrid().SetColumns(0, width, 0).SetRows(0, height, 0).
		AddItem(content, 1, 1, 1, 1, 0, 0, true)
	// Clamp fixed dialog tracks to the actual viewport, including small terminals.
	frame.SetDrawFunc(func(screen tcell.Screen, x, y, availableWidth, availableHeight int) (int, int, int, int) {
		frame.SetColumns(0, min(width, availableWidth), 0).SetRows(0, min(height, availableHeight), 0)
		return x, y, availableWidth, availableHeight
	})
	frame.SetMouseCapture(func(action tview.MouseAction, event *tcell.EventMouse) (tview.MouseAction, *tcell.EventMouse) {
		x, y := event.Position()
		left, top, width, height := content.GetRect()
		if x < left || y < top || x >= left+width || y >= top+height {
			return tview.MouseConsumed, nil
		}
		return action, event
	})
	ui.pages.AddPage("dialog", &dialogLayer{frame}, true, true)
	ui.app.SetFocus(content)
}

func (ui *UI) closeDialog() {
	if ui.dialogCancel != nil {
		ui.dialogCancel()
		ui.dialogCancel = nil
	}
	ui.pages.RemovePage("dialog")
	ui.dialog = false
	ui.dialogBody = nil
	ui.app.SetFocus(ui.dialogFocus)
}

func (ui *UI) modal(title, message string, confirm func()) {
	text := ui.textView().SetText(clean(message))
	ui.messageDialog(title, text, confirm, 68, 20)
}

func (ui *UI) messageDialog(title string, text *tview.TextView, confirm func(), width, height int) *tview.Form {
	buttons := tview.NewForm().SetButtonsAlign(tview.AlignCenter).
		SetButtonBackgroundColor(ui.palette.surface).SetButtonTextColor(ui.palette.text).
		SetButtonActivatedStyle(tcell.StyleDefault.Background(ui.palette.accent).Foreground(ui.palette.background))
	buttons.SetBackgroundColor(ui.palette.background)
	if confirm == nil {
		buttons.AddButton(ui.tr("Close", "关闭"), ui.closeDialog)
	} else {
		buttons.AddButton(ui.tr("Cancel", "取消"), ui.closeDialog).
			AddButton(ui.tr("Confirm", "确认"), func() { ui.closeDialog(); confirm() })
	}
	text.SetDoneFunc(func(tcell.Key) { ui.app.SetFocus(buttons) })
	buttons.SetCancelFunc(ui.closeDialog)
	panel := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(text, 0, 1, confirm == nil).AddItem(buttons, 3, 0, confirm != nil)
	ui.box(panel.Box, title)
	ui.openDialog(panel, width, height)
	return buttons
}

func dialogText(rows [][2]string, width int) string {
	width = max(1, width)
	labelWidth := 0
	for _, row := range rows {
		labelWidth = max(labelWidth, tview.TaggedStringWidth(escaped(row[0])))
	}
	labelWidth = min(labelWidth, max(1, (width-2)/2))
	var lines []string
	for _, row := range rows {
		if row[0] == "" || width < 12 {
			value := row[1]
			if row[0] != "" {
				value = row[0] + "\n" + value
			}
			if value == "" {
				lines = append(lines, "")
			} else {
				lines = append(lines, tview.WordWrap(escaped(value), width)...)
			}
			continue
		}
		labels := tview.WordWrap(escaped(row[0]), labelWidth)
		values := tview.WordWrap(escaped(row[1]), width-labelWidth-2)
		for index := 0; index < max(len(labels), len(values)); index++ {
			label, value := "", ""
			if index < len(labels) {
				label = labels[index]
			}
			if index < len(values) {
				value = values[index]
			}
			lines = append(lines, label+strings.Repeat(" ", labelWidth-tview.TaggedStringWidth(label)+2)+value)
		}
	}
	return strings.Join(lines, "\n")
}

func (ui *UI) informationDialog(title string, rows [][2]string) (*tview.Form, func([][2]string)) {
	text := ui.textView().SetDynamicColors(true).SetWrap(false)
	text.SetBorderPadding(0, 0, 1, 1)
	lastWidth := -1
	text.SetDrawFunc(func(_ tcell.Screen, x, y, width, height int) (int, int, int, int) {
		if width != lastWidth {
			text.SetText(dialogText(rows, width))
			lastWidth = width
		}
		return x, y, width, height
	})
	buttons := ui.messageDialog(title, text, nil, 78, 26)
	buttons.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyBacktab && buttons.GetButton(0).HasFocus() {
			ui.app.SetFocus(text)
			return nil
		}
		return event
	})
	return buttons, func(next [][2]string) {
		rows = next
		lastWidth = -1
	}
}

func (ui *UI) help() {
	ui.informationDialog(ui.tr(" Help ", " 帮助 "), [][2]string{
		{"", ui.tr("Search and navigation", "查询与导航")},
		{"/, Ctrl+K", ui.tr("Focus and clear search", "聚焦并清空搜索框")},
		{"Enter", ui.tr("Query typed text / activate selected word", "查询输入原文 / 打开选中词")},
		{"Tab / Shift+Tab", ui.tr("Next / previous pane", "下一个 / 上一个区域")},
		{"j / k", ui.tr("Down / up (also arrow keys)", "下移 / 上移，也可使用方向键")},
		{"g / G", ui.tr("First / last; top / bottom", "首项 / 末项；顶部 / 底部")},
		{"Ctrl+D / Ctrl+U", ui.tr("Page down / up", "下翻页 / 上翻页")},
		{"h / l", ui.tr("Back / forward (also Alt+Left / Alt+Right)", "后退 / 前进，也可使用 Alt+Left / Alt+Right")},
		{"u / p", ui.tr("UK / US pronunciation of the headword", "当前词条的英音 / 美音")},
		{"", ""},
		{"", ui.tr("History and application", "历史与应用")},
		{"F2", ui.tr("Show / hide recent queries", "显示 / 隐藏最近查询")},
		{"e / d / D", ui.tr("Edit / delete / clear (history list only)", "编辑 / 删除 / 清空，仅在历史列表中生效")},
		{"F3 / ,", ui.tr("Settings (unavailable during downloads)", "设置，下载期间不可用")},
		{"F4", ui.tr("About and local file paths", "关于与本地文件位置")},
		{"F5", ui.tr("Cancel / retry initialization only", "仅取消 / 重试数据初始化")},
		{"F1 / ?", ui.tr("Help", "帮助")},
		{"Esc", ui.tr("Leave input / close dialog or dropdown", "离开输入框 / 关闭对话框或下拉菜单")},
		{"q / Ctrl+C", ui.tr("Quit and stop playback", "退出并停止播放")},
		{"", ""},
		{"", ui.tr("Notes", "使用说明")},
		{"", ui.tr("Letter and punctuation shortcuts apply outside input fields and dialogs. Ctrl+C also quits from dialogs.", "字母和标点快捷键仅在输入框及对话框外生效。Ctrl+C 在对话框内也可退出。")},
		{"", ui.tr("In Help / About: arrows, PgUp / PgDn or mouse wheel scroll; Home / End jump to top / bottom. Tab selects Close; Esc closes directly.", "在帮助 / 关于中：方向键、PgUp / PgDn 或滚轮滚动，Home / End 跳至首尾；Tab 选中关闭按钮，Esc 直接关闭。")},
		{"", ui.tr("Enter in search always queries your text, not a completion. Editing history does not change the dictionary.", "搜索框中按 Enter 始终查询输入原文，不会自动选择补全词。编辑历史不会修改词典。")},
		{"", ui.tr("Mouse clicks select controls and words. Your terminal may require Shift to select / copy text.", "可用鼠标点击控件与词条；终端选择 / 复制文本可能需要按住 Shift。")},
		{"", ui.tr("Clipboard monitoring is opt-in in Settings. Press Esc to leave search; it pauses during input, dialogs, playback and recent activity. It never plays audio or saves clipboard queries to history.", "剪贴板监控需在设置中开启。按 Esc 离开搜索框；输入、弹窗、播放及刚操作界面时暂停。不会自动发音，也不会将剪贴板查询写入历史。")},
		{"", ui.tr("Missing data downloads on startup. To reinstall pinned data-3 assets, restart with -update-data; add -download to exit when done. Use -config <path> for a separate profile.", "启动时自动下载缺失数据。重新运行时加 -update-data 可重装固定的 data-3 数据，加 -download 可在完成后退出；-config <路径> 可使用独立配置。")},
	})
}

func (ui *UI) about() {
	if ui.dialog {
		return
	}
	rows := [][2]string{
		{"", "stupidict"},
		{"", ui.tr("Offline English-Chinese dictionary", "离线英汉词典")},
		{"", ""},
	}
	for index, line := range strings.Split(ui.version, "\n") {
		label, value, found := strings.Cut(line, ": ")
		if index == 0 {
			label, value = ui.tr("Version", "版本"), line
		} else if !found {
			label, value = "", line
		}
		rows = append(rows, [2]string{label, value})
	}
	summaryIndex := len(rows)
	audio := ui.config.Audio
	if audio == "" {
		audio = ui.tr("Not configured", "未配置")
	}
	rows = append(rows, [][2]string{
		{ui.tr("Stack", "技术"), "Go + tview + SQLite"},
		{ui.tr("License", "许可"), "MIT"},
		{ui.tr("Author", "作者"), "mingcheng"},
		{ui.tr("Upstream", "上游"), "cholf5/stupid-dict"},
		{"", projectURL},
		{"", ""},
		{"", ui.tr("Local files", "本地文件")},
		{ui.tr("Config", "配置文件"), ui.configPath},
		{ui.tr("History", "历史文件"), filepath.Join(filepath.Dir(ui.configPath), "history.db")},
		{ui.tr("Dictionary", "词典文件"), ui.config.Dictionary},
		{ui.tr("Audio", "语音文件"), audio},
		{"", ""},
		{"", ui.tr("Lookups and pronunciation use local data. Audio requires an installed audio pack and a build with native playback (CGO); there is no text-to-speech fallback.", "查询与发音使用本地数据。发音需要已安装的语音包及支持原生播放（CGO）的构建，不提供语音合成回退。")},
		{"", ui.tr("Data source: cholf5/stupid-dict, release data-3. Data updates reinstall this pinned release, not the application. Close this dialog, then press F1 for Help.", "数据来源：cholf5/stupid-dict 的 data-3 发布。数据更新会重装此固定版本，不会升级应用。关闭此对话框后按 F1 可打开帮助。")},
	}...)
	buttons, setRows := ui.informationDialog(ui.tr(" About ", " 关于 "), rows)
	panel := ui.dialogBody
	ctx, cancel := context.WithCancel(ui.ctx)
	ui.dialogCancel = cancel
	var info *client.DataInfo
	browserStatus := ""
	update := func() {
		content := append([][2]string(nil), rows[:summaryIndex]...)
		content = append(content, ui.dataRows(info)...)
		content = append(content, rows[summaryIndex:]...)
		if browserStatus != "" {
			content = append(content, [2]string{ui.tr("Browser", "浏览器"), browserStatus})
		}
		setRows(content)
	}
	update()
	buttons.AddButton(ui.tr("Open GitHub", "打开 GitHub"), func() {
		button := buttons.GetButton(1)
		button.SetDisabled(true)
		browserStatus = ui.tr("Opening...", "正在打开...")
		update()
		ui.work(ctx, func(ctx context.Context) func() {
			openCtx, stop := context.WithTimeout(ctx, 10*time.Second)
			defer stop()
			err := ui.openURL(openCtx, projectURL)
			return func() {
				if ui.dialogBody != panel {
					return
				}
				button.SetDisabled(false)
				if err != nil {
					browserStatus = ui.tr("Could not open browser; open this URL manually: ", "无法打开浏览器，请手动打开：") + projectURL + " (" + clean(err.Error()) + ")"
				} else {
					browserStatus = ui.tr("Link sent to your default browser", "链接已交给默认浏览器")
				}
				update()
				ui.status.SetText(browserStatus)
			}
		})
	})
	ui.work(ctx, func(ctx context.Context) func() {
		queryCtx, stop := context.WithTimeout(ctx, 10*time.Second)
		defer stop()
		result := ui.backend.DataInfo(queryCtx)
		return func() {
			if ui.dialogBody == panel {
				info = &result
				update()
			}
		}
	})
}

func (ui *UI) dataRows(info *client.DataInfo) [][2]string {
	rows := [][2]string{{"", ""}, {"", ui.tr("Data summary", "数据概况")}}
	if info == nil {
		return append(rows, [2]string{ui.tr("Entries", "词条数量"), ui.tr("Loading...", "加载中...")}, [2]string{"", ""})
	}
	count := fmt.Sprintf("%d", info.Entries)
	if info.CountError != nil {
		count = ui.tr("Unavailable: ", "无法读取：") + info.CountError.Error()
	}
	rows = append(rows, [2]string{ui.tr("Entries", "词条数量"), count})
	for _, asset := range []struct {
		name string
		info client.FileInfo
	}{
		{ui.tr("Dictionary", "词典"), info.Dictionary},
		{ui.tr("Audio", "语音"), info.Audio},
	} {
		if asset.name == ui.tr("Audio", "语音") && ui.config.Audio == "" {
			rows = append(rows, [2]string{asset.name, ui.tr("Not configured", "未配置")})
		} else if asset.info.Err != nil {
			state := ui.tr("Unavailable: ", "无法读取：") + asset.info.Err.Error()
			if errors.Is(asset.info.Err, os.ErrNotExist) {
				state = ui.tr("Not installed", "未安装")
			}
			rows = append(rows, [2]string{asset.name, state})
		} else {
			rows = append(rows,
				[2]string{asset.name + ui.tr(" size", "大小"), fmt.Sprintf("%.1f MiB (%d %s)", float64(asset.info.Size)/(1024*1024), asset.info.Size, ui.tr("bytes", "字节"))},
				[2]string{asset.name + ui.tr(" modified", "修改时间"), asset.info.Modified.Local().Format(time.RFC3339)},
			)
		}
	}
	return rows
}

func (ui *UI) form(title string) *tview.Form {
	form := tview.NewForm().SetItemPadding(0).SetFieldBackgroundColor(ui.palette.surface).
		SetFieldTextColor(ui.palette.text).SetLabelColor(ui.palette.accent).
		SetButtonBackgroundColor(ui.palette.surface).SetButtonTextColor(ui.palette.text).
		SetButtonActivatedStyle(tcell.StyleDefault.Background(ui.palette.accent).Foreground(ui.palette.background))
	ui.box(form.Box, title)
	form.SetCancelFunc(ui.closeDialog)
	return form
}

func (ui *UI) settings() {
	if ui.dialog || ui.configuring || ui.downloading {
		return
	}
	config := ui.config
	form := ui.form(ui.tr(" Settings ", " 设置 "))
	index := func(options []string, selected string) int {
		for position, option := range options {
			if option == selected {
				return position
			}
		}
		return 0
	}
	themes, languages := []string{"system", "light", "dark"}, []string{"system", "zh", "en"}
	form.AddDropDown(ui.tr("Theme", "主题"), []string{ui.tr("Terminal default", "终端默认"), ui.tr("Light", "浅色"), ui.tr("Dark", "深色")}, index(themes, config.Theme), func(_ string, selected int) { config.Theme = themes[selected] })
	form.AddDropDown(ui.tr("Language", "语言"), []string{ui.tr("System", "跟随系统"), "简体中文", "English"}, index(languages, config.Language), func(_ string, selected int) { config.Language = languages[selected] })
	form.AddInputField(ui.tr("Dictionary DB", "词典 DB"), config.Dictionary, 0, nil, func(value string) { config.Dictionary = value })
	form.AddInputField(ui.tr("Audio DB", "语音 DB"), config.Audio, 0, nil, func(value string) { config.Audio = value })
	glyphs := []string{"plain", "nerd", "emoji"}
	form.AddDropDown(ui.tr("Icons", "图标"), []string{ui.tr("Plain", "普通"), "Nerd Font", "Emoji"}, index(glyphs, config.Glyphs), func(_ string, selected int) { config.Glyphs = glyphs[selected] })
	form.AddCheckbox(ui.tr("Monitor clipboard (quiet)", "监控剪贴板（静默）"), config.MonitorClipboard, func(enabled bool) { config.MonitorClipboard = enabled })
	form.AddButton(ui.tr("Save", "保存"), func() {
		ui.configuring = true
		ui.invalidate()
		ui.closeDialog()
		ui.status.SetText(ui.tr("Saving...", "保存中..."))
		ctx, cancel := context.WithTimeout(ui.ctx, 15*time.Second)
		ui.work(ctx, func(ctx context.Context) func() {
			defer cancel()
			err := ui.backend.Configure(ctx, config)
			return func() {
				ui.configuring = false
				if err != nil {
					ui.fail(err)
					return
				}
				nextConfig := ui.backend.Config()
				changedDatabase := nextConfig.Dictionary != ui.config.Dictionary
				ui.invalidate()
				ui.config = nextConfig
				if ready, ok := ui.backend.(interface{ Ready() bool }); ok {
					ui.dictionaryPending = !ready.Ready()
				}
				ui.build()
				if changedDatabase {
					ui.current = nil
					ui.navigation = client.Navigation{}
				}
				if ui.current != nil {
					ui.setSearch(ui.current.Query)
					ui.render(*ui.current)
				}
				ui.updateNavigation()
				ui.refreshHistory()
				ui.status.SetText(ui.tr("Settings saved", "设置已保存"))
				ui.startClipboard()
			}
		})
	}).AddButton(ui.tr("Cancel", "取消"), ui.closeDialog)
	ui.openDialog(form, 72, 15)
}

func (ui *UI) selectedRecord() (profile.Record, bool) {
	index := ui.recents.GetCurrentItem()
	if index < 0 || index >= len(ui.records) {
		return profile.Record{}, false
	}
	return ui.records[index], true
}

func (ui *UI) editHistory() {
	record, ok := ui.selectedRecord()
	if !ok {
		return
	}
	query := record.Query
	form := ui.form(ui.tr(" Edit history ", " 编辑历史 "))
	form.AddInputField(ui.tr("Query", "查询词"), query, 0, nil, func(value string) { query = value })
	form.AddButton(ui.tr("Save", "保存"), func() {
		if strings.TrimSpace(query) == "" {
			return
		}
		ui.closeDialog()
		ui.changeHistory(func() error { return ui.backend.EditHistory(record.ID, query) })
	}).AddButton(ui.tr("Cancel", "取消"), ui.closeDialog)
	ui.openDialog(form, 60, 7)
}

func (ui *UI) deleteHistory() {
	record, ok := ui.selectedRecord()
	if !ok {
		return
	}
	ui.modal(ui.tr(" Delete record ", " 删除记录 "), record.Query, func() {
		ui.changeHistory(func() error { return ui.backend.DeleteHistory(record.ID) })
	})
}

func (ui *UI) clearHistory() {
	ui.modal(ui.tr(" Clear history ", " 清空历史 "), ui.tr("Delete all recent queries? This cannot be undone.", "删除所有最近查询？此操作无法撤销。"), func() { ui.changeHistory(ui.backend.ClearHistory) })
}

func (ui *UI) changeHistory(action func() error) {
	ui.queueHistory(func() func() {
		err := action()
		return func() {
			if err != nil {
				ui.fail(err)
			}
			ui.refreshHistory()
		}
	})
}
