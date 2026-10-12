// Package tui is the terminal adapter. Widget state belongs exclusively to the
// tview event loop; workers deliver immutable results through a bounded mailbox.
package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"
	dictionary "github.com/mingcheng/stupidict"
	"github.com/mingcheng/stupidict/internal/assets"
	"github.com/mingcheng/stupidict/internal/client"
	"github.com/mingcheng/stupidict/internal/clipboard"
	"github.com/mingcheng/stupidict/internal/profile"
	"github.com/rivo/tview"
)

type Backend interface {
	Lookup(context.Context, string) (dictionary.LookupResult, error)
	Suggest(context.Context, string) ([]string, error)
	Accept(dictionary.LookupResult) error
	History() ([]profile.Record, error)
	EditHistory(int64, string) error
	DeleteHistory(int64) error
	ClearHistory() error
	Config() profile.Config
	Configure(context.Context, profile.Config) error
	Audio(context.Context, string, dictionary.Accent) ([]byte, error)
	DataInfo(context.Context) client.DataInfo
}

type Speaker interface {
	Play(context.Context, []byte, string, dictionary.Accent) error
	Stop()
}

type UI struct {
	app                                                                *tview.Application
	backend                                                            Backend
	speaker                                                            Speaker
	config                                                             profile.Config
	configPath                                                         string
	version                                                            string
	openURL                                                            func(context.Context, string) error
	palette                                                            palette
	pages                                                              *tview.Pages
	root                                                               *tview.Grid
	body                                                               *tview.Flex
	detail                                                             *tview.Flex
	dictionaryPane                                                     *tview.Flex
	pronunciation                                                      *tview.Flex
	pronunciationAvailable                                             [2]bool
	pronunciationGeneration                                            uint64
	pronunciationCancel                                                context.CancelFunc
	side                                                               *tview.Pages
	search                                                             *tview.InputField
	suggestions                                                        *tview.List
	recents                                                            *tview.List
	results                                                            *tview.TextView
	headword                                                           *tview.TextView
	links                                                              *tview.List
	status                                                             *tview.TextView
	statusLine                                                         *tview.Flex
	clipboardStatus                                                    *tview.TextView
	clipboard                                                          clipboardMonitor
	back, forward                                                      *tview.Button
	quit                                                               *tview.Button
	british, american                                                  *tview.Button
	focus                                                              []tview.Primitive
	records                                                            []profile.Record
	navigation                                                         client.Navigation
	current                                                            *dictionary.LookupResult
	historyVisible                                                     bool
	dialog                                                             bool
	dialogFocus                                                        tview.Primitive
	dialogBody                                                         tview.Primitive
	dialogCancel                                                       context.CancelFunc
	configuring                                                        bool
	suppress                                                           bool
	narrow                                                             bool
	generation, suggestGeneration, historyGeneration, speechGeneration uint64
	queryCancel, suggestCancel, speechCancel                           context.CancelFunc
	ctx                                                                context.Context
	cancel                                                             context.CancelFunc
	updates                                                            chan func()
	historyMu                                                          sync.Mutex
	historyJobs                                                        []func() func()
	historyWake                                                        chan struct{}
	workers                                                            sync.WaitGroup
	ready                                                              chan struct{}
	initializer                                                        func(context.Context, bool, assets.Reporter) error
	initialForce, dictionaryPending, downloading, downloadForce        bool
	downloadCancel                                                     context.CancelFunc
	downloadInfo                                                       *tview.TextView
	downloadButton                                                     *tview.Button
	downloadPane                                                       *tview.Flex
	footer                                                             *tview.Flex
	downloadProgress                                                   assets.Progress
	downloadError                                                      error
}

func New(backend Backend, speaker Speaker, configPath, version string) *UI {
	ui := &UI{app: tview.NewApplication(), backend: backend, speaker: speaker,
		config: backend.Config(), configPath: configPath, version: version, openURL: openBrowserURL,
		clipboard: clipboardMonitor{open: clipboard.Open},
		updates:   make(chan func(), 64), historyWake: make(chan struct{}, 1), ready: make(chan struct{})}
	ui.build()
	ui.app.EnableMouse(true).EnablePaste(true).SetInputCapture(ui.key)
	ui.app.SetMouseCapture(func(event *tcell.EventMouse, action tview.MouseAction) (*tcell.EventMouse, tview.MouseAction) {
		if action != tview.MouseMove {
			ui.clipboardActivity()
		}
		return event, action
	})
	return ui
}

// SetScreen permits deterministic cell-level and mouse tests without a real TTY.
func (ui *UI) SetScreen(screen tcell.Screen) { ui.app.SetScreen(screen) }

func (ui *UI) Run(ctx context.Context) error {
	ui.ctx, ui.cancel = context.WithCancel(ctx)
	defer ui.cancel()
	ui.workers.Add(1)
	go func() {
		defer ui.workers.Done()
		// Drain accepted history operations even on exit. FIFO ordering prevents a
		// pending visit from reappearing after the user clears the recent list.
		for range ui.historyWake {
			ui.historyMu.Lock()
			jobs := ui.historyJobs
			ui.historyJobs = nil
			ui.historyMu.Unlock()
			for _, task := range jobs {
				if update := task(); update != nil {
					select {
					case ui.updates <- update:
					case <-ui.ctx.Done():
					}
				}
			}
		}
	}()
	var start sync.Once
	ui.app.SetBeforeDrawFunc(func(screen tcell.Screen) bool {
		width, height := screen.Size()
		ui.resize(width, height)
		ui.renderClipboardStatus()
		start.Do(func() {
			go ui.dispatch()
			ui.refreshHistory()
			ui.startClipboard()
			if ui.initializer != nil {
				ui.initialize(ui.initialForce)
			}
			close(ui.ready)
		})
		return false
	})
	err := ui.app.Run()
	ui.cancel()
	close(ui.historyWake)
	ui.speaker.Stop()
	ui.workers.Wait()
	return err
}

// Only this goroutine queues widget updates. Shutdown is queued behind the
// current update, so workers never wait on tview after the event loop stops.
func (ui *UI) dispatch() {
	for {
		select {
		case <-ui.ctx.Done():
			ui.app.QueueUpdateDraw(func() { ui.app.Stop() })
			return
		case update := <-ui.updates:
			ui.app.QueueUpdateDraw(func() {
				if ui.ctx.Err() == nil {
					update()
				}
			})
		}
	}
}

func (ui *UI) work(ctx context.Context, task func(context.Context) func()) {
	ui.workers.Add(1)
	go func() {
		defer ui.workers.Done()
		update := task(ctx)
		if update == nil {
			return
		}
		select {
		case ui.updates <- update:
		case <-ui.ctx.Done():
		}
	}()
}

func (ui *UI) build() {
	ui.palette = colors(ui.config.Theme)
	ui.pages = tview.NewPages()
	ui.status = ui.textView().SetScrollable(false).SetWrap(false)
	ui.status.SetText(ui.tr("Ready", "就绪"))
	ui.statusLine = tview.NewFlex().AddItem(ui.status, 0, 1, false)
	ui.clipboardStatus = ui.textView().SetScrollable(false).SetWrap(false).SetTextColor(ui.palette.muted)
	if ui.config.MonitorClipboard {
		ui.statusLine.AddItem(ui.clipboardStatus, 24, 0, false)
	}
	ui.search = tview.NewInputField().SetLabel(" " + ui.iconLabel("\uf002", ui.tr("Search", "搜索")) + "  ").SetFieldWidth(0)
	ui.search.SetAcceptanceFunc(func(text string, _ rune) bool { return len(text) <= 4096 && !strings.ContainsAny(text, "\r\n\t") })
	ui.search.SetFieldBackgroundColor(ui.palette.surface).SetFieldTextColor(ui.palette.text).SetLabelColor(ui.palette.accent)
	ui.box(ui.search.Box, "")
	ui.search.SetChangedFunc(func(text string) {
		if !ui.suppress {
			ui.clipboardActivity()
			ui.suggest(text)
		}
	})
	ui.search.SetDoneFunc(func(key tcell.Key) {
		if key == tcell.KeyEnter {
			ui.lookup(ui.search.GetText())
		}
	})
	ui.suggestions = ui.list().ShowSecondaryText(false)
	ui.box(ui.suggestions.Box, " "+ui.iconLabel("\uf0ca", ui.tr("Suggestions", "候选词"))+" ")
	ui.suggestions.SetSelectedFunc(func(_ int, text, _ string, _ rune) { ui.lookup(text) })
	ui.recents = ui.list().ShowSecondaryText(false)
	ui.box(ui.recents.Box, " "+ui.iconLabel("\uf1da", ui.tr("Recent queries", "最近查询"))+" ")
	ui.recents.SetSelectedFunc(func(index int, _, _ string, _ rune) {
		if index < len(ui.records) {
			ui.lookup(ui.records[index].Query)
		}
	})
	historyActions := tview.NewFlex()
	for _, action := range []struct {
		label string
		run   func()
	}{
		{ui.tr("Edit", "编辑"), ui.editHistory}, {ui.tr("Delete", "删除"), ui.deleteHistory}, {ui.tr("Clear", "清空"), ui.clearHistory},
	} {
		button := tview.NewButton(action.label).SetSelectedFunc(action.run).
			SetStyle(tcell.StyleDefault.Background(ui.palette.surface).Foreground(ui.palette.text)).
			SetActivatedStyle(tcell.StyleDefault.Background(ui.palette.accent).Foreground(ui.palette.background))
		historyActions.AddItem(button, 0, 1, false)
	}
	historyPane := tview.NewFlex().SetDirection(tview.FlexRow).AddItem(ui.recents, 0, 1, true).AddItem(historyActions, 1, 0, false)
	ui.side = tview.NewPages().AddPage("suggestions", ui.suggestions, true, true).AddPage("history", historyPane, true, false)
	ui.results = ui.textView().SetDynamicColors(true)
	ui.headword = ui.textView().SetDynamicColors(true).SetScrollable(false).SetWrap(false).SetTextColor(ui.palette.accent)
	ui.links = ui.list().ShowSecondaryText(false)
	ui.box(ui.links.Box, " "+ui.iconLabel("\uf0c1", ui.tr("Related / alternatives", "关联词 / 建议"))+" ")
	ui.links.SetSelectedFunc(func(_ int, text, _ string, _ rune) { ui.lookup(text) })
	toolbar := tview.NewFlex()
	add := func(target *tview.Flex, label string, width int, action func()) *tview.Button {
		button := tview.NewButton(label).SetSelectedFunc(action)
		button.SetStyle(tcell.StyleDefault.Background(ui.palette.surface).Foreground(ui.palette.text))
		button.SetActivatedStyle(tcell.StyleDefault.Background(ui.palette.accent).Foreground(ui.palette.background))
		button.SetDisabledStyle(tcell.StyleDefault.Background(ui.palette.surface).Foreground(ui.palette.muted))
		buttonWidth := max(width, tview.TaggedStringWidth(label)+2)
		if target == toolbar && ui.config.Glyphs == "emoji" {
			buttonWidth--
		}
		target.AddItem(button, buttonWidth, 0, false)
		return button
	}
	backLabel, forwardLabel, helpLabel := "<", ">", "?"
	playLabel := ui.tr("Play", "播放")
	if glyph := ui.iconGlyph("\uf028"); glyph != "" {
		backLabel, forwardLabel, helpLabel = ui.iconGlyph("\uf060"), ui.iconGlyph("\uf061"), ui.iconGlyph("\uf059")
		playLabel = glyph
	}
	ui.back = add(toolbar, backLabel, 3, func() { ui.move(-1) })
	ui.forward = add(toolbar, forwardLabel, 3, func() { ui.move(1) })
	add(toolbar, ui.iconLabel("\uf1da", ui.tr("History", "历史")), 9, ui.showHistory)
	toolbar.AddItem(nil, 2, 0, false)
	ui.pronunciation = tview.NewFlex().SetDirection(tview.FlexRow)
	ui.pronunciationAvailable = [2]bool{}
	ui.british = add(ui.pronunciation, playLabel+" UK", 10, func() { ui.play(dictionary.British) }).SetDisabled(true)
	ui.american = add(ui.pronunciation, playLabel+" US", 10, func() { ui.play(dictionary.American) }).SetDisabled(true)
	ui.pronunciation.Clear()
	for _, button := range []*tview.Button{ui.british, ui.american} {
		button.SetStyle(tcell.StyleDefault.Background(ui.palette.surface).Foreground(ui.palette.accent).Bold(true))
		button.SetMouseCapture(func(action tview.MouseAction, event *tcell.EventMouse) (tview.MouseAction, *tcell.EventMouse) {
			if action == tview.MouseLeftDoubleClick {
				action = tview.MouseLeftClick
			}
			return action, event
		})
	}
	ui.dictionaryPane = tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(ui.headword, 1, 0, false).AddItem(ui.results, 0, 1, false)
	ui.box(ui.dictionaryPane.Box, " "+ui.iconLabel("\uf02d", ui.tr("Dictionary", "词典"))+" ")
	ui.detail = tview.NewFlex().SetDirection(tview.FlexRow).AddItem(ui.dictionaryPane, 0, 1, false).AddItem(ui.links, 6, 0, false)
	ui.body = tview.NewFlex().AddItem(ui.side, 26, 0, false).AddItem(ui.detail, 0, 1, false)
	add(toolbar, ui.iconLabel("\uf013", ui.tr("Settings", "设置")), 10, ui.settings)
	add(toolbar, ui.iconLabel("\uf05a", ui.tr("About", "关于")), 8, ui.about)
	add(toolbar, helpLabel, 3, ui.help)
	toolbar.AddItem(nil, 0, 1, false)
	ui.quit = add(toolbar, ui.iconLabel("\uf08b", ui.tr("Quit", "退出")), 6, func() { ui.cancel() })
	title := ui.textView().SetScrollable(false).SetTextColor(ui.palette.accent)
	icon := ui.iconGlyph("\uf02d")
	if icon != "" {
		icon += "  "
	}
	title.SetText(icon + "stupidict  /  " + ui.tr("Offline dictionary", "离线英汉词典"))
	ui.root = tview.NewGrid().SetRows(1, 1, 3, 0, 1).SetColumns(0)
	ui.root.SetBackgroundColor(ui.palette.background)
	downloadWidth := max(8, tview.TaggedStringWidth(ui.iconLabel("\uf04d", ui.tr("Cancel", "取消")))+2)
	ui.downloadInfo = ui.textView().SetScrollable(false).SetWrap(false)
	ui.downloadPane = tview.NewFlex().AddItem(ui.downloadInfo, 0, 1, false)
	ui.downloadButton = add(ui.downloadPane, ui.iconLabel("\uf04d", ui.tr("Cancel", "取消")), downloadWidth, ui.toggleDownload)
	ui.footer = tview.NewFlex().SetDirection(tview.FlexRow).AddItem(ui.statusLine, 1, 0, false)
	ui.root.AddItem(title, 0, 0, 1, 1, 0, 0, false).AddItem(toolbar, 1, 0, 1, 1, 0, 0, false).
		AddItem(ui.search, 2, 0, 1, 1, 0, 0, true).AddItem(ui.body, 3, 0, 1, 1, 0, 0, false).AddItem(ui.footer, 4, 0, 1, 1, 0, 0, false)
	ui.pages.AddPage("main", ui.root, true, true)
	ui.app.SetRoot(ui.pages, true).SetFocus(ui.search)
	ui.focus = []tview.Primitive{ui.search, ui.suggestions, ui.results, ui.british, ui.american, ui.links}
	ui.historyVisible, ui.narrow = false, false
	ui.dialog, ui.dialogBody, ui.dialogFocus = false, nil, nil
	if ui.dialogCancel != nil {
		ui.dialogCancel()
		ui.dialogCancel = nil
	}
	ui.updateNavigation()
	ui.renderDownload()
}

func (ui *UI) resize(width, height int) {
	narrow := width < 80
	ui.narrow = narrow
	if narrow {
		ui.body.SetDirection(tview.FlexRow).ResizeItem(ui.side, min(6, max(3, height/5)), 0)
	} else {
		ui.body.SetDirection(tview.FlexColumn).ResizeItem(ui.side, 26, 0)
	}
	ui.detail.ResizeItem(ui.links, min(6, max(3, (height-6)/4)), 0)
}

func (ui *UI) lookup(query string) {
	if ui.dictionaryPending {
		return
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return
	}
	ui.invalidate()
	generation := ui.generation
	ctx, cancel := context.WithTimeout(ui.ctx, 15*time.Second)
	ui.queryCancel = cancel
	ui.setSearch(query)
	ui.status.SetText(ui.tr("Searching...", "查询中..."))
	ui.app.SetFocus(ui.results)
	ui.work(ctx, func(ctx context.Context) func() {
		defer cancel()
		result, err := ui.backend.Lookup(ctx, query)
		return func() {
			if generation != ui.generation {
				return
			}
			ui.queryCancel = nil
			if err != nil {
				ui.fail(err)
				return
			}
			ui.navigation.Push(result)
			ui.render(result)
			ui.changeHistory(func() error { return ui.backend.Accept(result) })
		}
	})
}

func (ui *UI) suggest(query string) {
	if ui.dictionaryPending {
		return
	}
	// Typing invalidates pending lookup results as well as older completions.
	ui.invalidate()
	generation := ui.suggestGeneration
	ui.historyVisible = false
	ui.side.SwitchToPage("suggestions")
	ui.focus[1] = ui.suggestions
	ui.suggestions.Clear()
	if strings.TrimSpace(query) == "" {
		return
	}
	ctx, cancel := context.WithTimeout(ui.ctx, 15*time.Second)
	ui.suggestCancel = cancel
	ui.work(ctx, func(ctx context.Context) func() {
		defer cancel()
		timer := time.NewTimer(120 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}
		words, err := ui.backend.Suggest(ctx, query)
		return func() {
			if generation != ui.suggestGeneration {
				return
			}
			if err != nil {
				ui.fail(err)
				return
			}
			ui.suggestions.Clear()
			for _, word := range words {
				ui.suggestions.AddItem(word, "", 0, nil)
			}
		}
	})
}

func (ui *UI) invalidate() {
	ui.cancelClipboardQuery()
	ui.clipboard.pending = ""
	ui.generation++
	ui.suggestGeneration++
	if ui.queryCancel != nil {
		ui.queryCancel()
		ui.queryCancel = nil
	}
	if ui.suggestCancel != nil {
		ui.suggestCancel()
	}
}

func (ui *UI) setSearch(query string) {
	ui.suppress = true
	ui.search.SetText(query)
	ui.suppress = false
}

func (ui *UI) move(delta int) {
	ui.invalidate()
	if result, ok := ui.navigation.Move(delta); ok {
		ui.setSearch(result.Query)
		ui.render(result)
	}
}

func (ui *UI) updateNavigation() {
	ui.back.SetDisabled(!ui.navigation.CanBack())
	ui.forward.SetDisabled(!ui.navigation.CanForward())
}

func (ui *UI) queueHistory(task func() func()) {
	ui.historyMu.Lock()
	ui.historyJobs = append(ui.historyJobs, task)
	ui.historyMu.Unlock()
	select {
	case ui.historyWake <- struct{}{}:
	default:
	}
}

func (ui *UI) refreshHistory() {
	ui.historyGeneration++
	generation := ui.historyGeneration
	ui.queueHistory(func() func() {
		records, err := ui.backend.History()
		return func() {
			if generation != ui.historyGeneration {
				return
			}
			if err != nil {
				ui.fail(err)
				return
			}
			selected := ui.recents.GetCurrentItem()
			ui.records = records
			ui.recents.Clear()
			for _, record := range records {
				ui.recents.AddItem(record.Query, "", 0, nil)
			}
			if len(records) > 0 {
				ui.recents.SetCurrentItem(selected)
			}
		}
	})
}

func (ui *UI) showHistory() {
	ui.historyVisible = !ui.historyVisible
	if ui.historyVisible {
		ui.side.SwitchToPage("history")
		ui.focus[1] = ui.recents
		ui.app.SetFocus(ui.recents)
		ui.refreshHistory()
	} else {
		ui.side.SwitchToPage("suggestions")
		ui.focus[1] = ui.suggestions
		ui.app.SetFocus(ui.suggestions)
	}
}

func (ui *UI) play(accent dictionary.Accent) {
	if ui.current == nil || ui.current.Primary == nil {
		return
	}
	if ui.speechCancel != nil {
		ui.speechCancel()
	}
	ui.speaker.Stop()
	ui.speechGeneration++
	generation := ui.speechGeneration
	word := ui.current.Primary.Word
	ctx, cancel := context.WithCancel(ui.ctx)
	ui.speechCancel = cancel
	ui.status.SetText(ui.tr("Playing ", "播放 ") + word + " / " + string(accent))
	ui.work(ctx, func(ctx context.Context) func() {
		defer cancel()
		audio, err := ui.backend.Audio(ctx, word, accent)
		if err == nil {
			err = ui.speaker.Play(ctx, audio, word, accent)
		}
		return func() {
			if generation != ui.speechGeneration {
				return
			}
			ui.speechCancel = nil
			if errors.Is(err, dictionary.ErrAudioNotFound) {
				ui.status.SetText(ui.tr("Audio unavailable; install or update the audio pack", "暂无发音，请安装或更新发音包"))
			} else if err != nil {
				ui.fail(err)
			} else {
				ui.status.SetText(ui.tr("Ready", "就绪"))
			}
		}
	})
}

func (ui *UI) fail(err error) { ui.status.SetText(ui.tr("Error: ", "错误：") + clean(err.Error())) }

func (ui *UI) key(event *tcell.EventKey) *tcell.EventKey {
	ui.clipboardActivity()
	if event.Key() == tcell.KeyCtrlC {
		ui.cancel()
		return nil
	}
	if ui.dialog {
		if event.Key() == tcell.KeyEsc {
			if dropdown, ok := ui.app.GetFocus().(*tview.DropDown); ok && dropdown.IsOpen() {
				return event
			}
			ui.closeDialog()
			return nil
		}
		return event
	}
	switch event.Key() {
	case tcell.KeyF1:
		ui.help()
		return nil
	case tcell.KeyF2:
		ui.showHistory()
		return nil
	case tcell.KeyF3:
		ui.settings()
		return nil
	case tcell.KeyF4:
		ui.about()
		return nil
	case tcell.KeyF5:
		ui.toggleDownload()
		return nil
	case tcell.KeyCtrlK:
		ui.app.SetFocus(ui.search)
		ui.search.SetText("")
		return nil
	case tcell.KeyTab, tcell.KeyBacktab:
		index := 0
		for position, item := range ui.focus {
			if item.HasFocus() {
				index = position
				break
			}
		}
		for {
			if event.Key() == tcell.KeyTab {
				index = (index + 1) % len(ui.focus)
			} else {
				index = (index + len(ui.focus) - 1) % len(ui.focus)
			}
			if (ui.focus[index] != ui.british || ui.pronunciationAvailable[0]) &&
				(ui.focus[index] != ui.american || ui.pronunciationAvailable[1]) {
				break
			}
		}
		ui.app.SetFocus(ui.focus[index])
		return nil
	case tcell.KeyEsc:
		ui.app.SetFocus(ui.results)
		return nil
	}
	if event.Modifiers()&tcell.ModAlt != 0 {
		if event.Key() == tcell.KeyLeft {
			ui.move(-1)
			return nil
		}
		if event.Key() == tcell.KeyRight {
			ui.move(1)
			return nil
		}
	}
	if ui.search.HasFocus() {
		return event
	}
	if event.Key() == tcell.KeyCtrlD {
		return tcell.NewEventKey(tcell.KeyPgDn, 0, tcell.ModNone)
	}
	if event.Key() == tcell.KeyCtrlU {
		return tcell.NewEventKey(tcell.KeyPgUp, 0, tcell.ModNone)
	}
	if event.Key() != tcell.KeyRune {
		return event
	}
	switch event.Rune() {
	case '/':
		ui.app.SetFocus(ui.search)
		ui.search.SetText("")
		return nil
	case 'q':
		ui.cancel()
		return nil
	case '?':
		ui.help()
		return nil
	case ',':
		ui.settings()
		return nil
	case 'h':
		ui.move(-1)
		return nil
	case 'l':
		ui.move(1)
		return nil
	case 'u':
		ui.play(dictionary.British)
		return nil
	case 'p':
		ui.play(dictionary.American)
		return nil
	case 'j':
		return tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone)
	case 'k':
		return tcell.NewEventKey(tcell.KeyUp, 0, tcell.ModNone)
	case 'g':
		return tcell.NewEventKey(tcell.KeyHome, 0, tcell.ModNone)
	case 'G':
		return tcell.NewEventKey(tcell.KeyEnd, 0, tcell.ModNone)
	case 'e':
		if ui.recents.HasFocus() {
			ui.editHistory()
		}
		return nil
	case 'd':
		if ui.recents.HasFocus() {
			ui.deleteHistory()
		}
		return nil
	case 'D':
		if ui.recents.HasFocus() {
			ui.clearHistory()
		}
		return nil
	}
	return event
}

func clean(text string) string {
	return strings.Map(func(character rune) rune {
		if character < 32 && character != '\n' && character != '\t' {
			return ' '
		}
		return character
	}, text)
}

func escaped(text string) string { return tview.Escape(clean(text)) }

func (ui *UI) summary(result dictionary.LookupResult) string {
	return fmt.Sprintf("%s  /  %s", result.Query, result.Tier)
}
