package tui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	dictionary "github.com/mingcheng/stupidict"
	"github.com/mingcheng/stupidict/internal/assets"
	"github.com/mingcheng/stupidict/internal/client"
	"github.com/mingcheng/stupidict/internal/profile"
	"github.com/rivo/tview"
)

type engineStub struct {
	lookup  func(context.Context, string) (dictionary.LookupResult, error)
	suggest func(context.Context, string) ([]string, error)
}

func (engine *engineStub) Close() error { return nil }
func (engine *engineStub) Lookup(ctx context.Context, query string) (dictionary.LookupResult, error) {
	if engine.lookup != nil {
		return engine.lookup(ctx, query)
	}
	return dictionary.LookupResult{Query: query, Tier: dictionary.Exact, Primary: &dictionary.Entry{Word: query, Translation: strings.Repeat("definition 中文\n", 100)}}, nil
}
func (engine *engineStub) Suggest(ctx context.Context, query string) ([]string, error) {
	if engine.suggest != nil {
		return engine.suggest(ctx, query)
	}
	return []string{"cat", "car"}, nil
}

type speakerStub struct {
	gate  sync.Mutex
	words []string
	stops int
}

func (speaker *speakerStub) Play(_ context.Context, _ []byte, word string, accent dictionary.Accent) error {
	speaker.gate.Lock()
	defer speaker.gate.Unlock()
	speaker.words = append(speaker.words, word+":"+string(accent))
	return nil
}
func (speaker *speakerStub) Stop() { speaker.gate.Lock(); defer speaker.gate.Unlock(); speaker.stops++ }

type harness struct {
	t       *testing.T
	ui      *UI
	screen  tcell.SimulationScreen
	service *client.Service
	speaker *speakerStub
}

func startUI(t *testing.T, engine *engineStub, setup ...func(*UI)) *harness {
	t.Helper()
	config := profile.Defaults()
	config.Language, config.Theme = "en", "dark"
	path := filepath.Join(t.TempDir(), "config.json")
	service, err := client.Open(context.Background(), path, config, func(context.Context, string) (client.Engine, error) { return engine, nil })
	if err != nil {
		t.Fatal(err)
	}
	speaker := &speakerStub{}
	ui := New(audioBackendStub{Backend: service}, speaker, path, "test")
	for _, configure := range setup {
		configure(ui)
	}
	screen := tcell.NewSimulationScreen("UTF-8")
	ui.SetScreen(screen)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ui.Run(ctx) }()
	select {
	case <-ui.ready:
	case err := <-done:
		t.Fatalf("start: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("start timed out")
	}
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("UI shutdown timed out")
		}
		service.Close()
	})
	harness := &harness{t, ui, screen, service, speaker}
	harness.resize(110, 34)
	return harness
}

func (harness *harness) onUI(action func()) {
	harness.t.Helper()
	done := make(chan struct{})
	select {
	case harness.ui.updates <- func() { action(); close(done) }:
	case <-time.After(3 * time.Second):
		harness.t.Fatal("mailbox stalled")
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		harness.t.Fatal("event loop stalled")
	}
}

func (harness *harness) wait(condition func() bool) {
	harness.t.Helper()
	deadline := time.NewTimer(4 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		var ok bool
		harness.onUI(func() { ok = condition() })
		if ok {
			return
		}
		select {
		case <-deadline.C:
			harness.t.Fatal("condition timed out")
		case <-ticker.C:
		}
	}
}

func (harness *harness) key(key tcell.Key, character rune) {
	harness.ui.app.QueueEvent(tcell.NewEventKey(key, character, tcell.ModNone))
}
func (harness *harness) click(x, y int) {
	harness.ui.app.QueueEvent(tcell.NewEventMouse(x, y, tcell.Button1, tcell.ModNone))
	harness.ui.app.QueueEvent(tcell.NewEventMouse(x, y, tcell.ButtonNone, tcell.ModNone))
}
func (harness *harness) resize(width, height int) {
	harness.screen.SetSize(width, height)
	harness.ui.app.QueueEvent(tcell.NewEventResize(width, height))
	harness.wait(func() bool { _, _, actual, _ := harness.ui.root.GetRect(); return actual == width })
}
func (harness *harness) query(query string) {
	harness.onUI(func() { harness.ui.setSearch(query); harness.ui.app.SetFocus(harness.ui.search) })
	harness.key(tcell.KeyEnter, 0)
	harness.wait(func() bool { return harness.ui.current != nil && harness.ui.current.Query == query })
}

func TestEnterUsesTypedTextAndNavigation(t *testing.T) {
	harness := startUI(t, &engineStub{})
	harness.onUI(func() { harness.ui.search.SetText("ca") })
	harness.wait(func() bool { return harness.ui.suggestions.GetItemCount() == 2 })
	harness.key(tcell.KeyEnter, 0)
	harness.wait(func() bool { return harness.ui.current != nil && harness.ui.current.Query == "ca" })
	harness.query("dog")
	harness.key(tcell.KeyRune, 'h')
	harness.wait(func() bool { return harness.ui.current.Query == "ca" })
	harness.key(tcell.KeyRune, 'l')
	harness.wait(func() bool { return harness.ui.current.Query == "dog" })
	harness.key(tcell.KeyCtrlK, 0)
	harness.key(tcell.KeyRune, 'j')
	harness.wait(func() bool { return harness.ui.search.GetText() == "j" })
}

func TestStaleLookupCannotRenderOrWriteHistory(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	engine := &engineStub{lookup: func(_ context.Context, query string) (dictionary.LookupResult, error) {
		if query == "old" {
			close(started)
			<-release
		}
		return dictionary.LookupResult{Query: query, Primary: &dictionary.Entry{Word: query}}, nil
	}}
	harness := startUI(t, engine)
	harness.onUI(func() { harness.ui.lookup("old") })
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("lookup did not start")
	}
	harness.query("new")
	close(release)
	harness.wait(func() bool { return len(harness.ui.records) == 1 })
	harness.onUI(func() {
		if harness.ui.current.Query != "new" || harness.ui.records[0].Query != "new" {
			t.Error("stale result accepted")
		}
	})
}

func TestMouseSelectionScrollingAndResponsiveLayout(t *testing.T) {
	harness := startUI(t, &engineStub{})
	harness.onUI(func() { harness.ui.search.SetText("c") })
	harness.wait(func() bool { return harness.ui.suggestions.GetItemCount() == 2 })
	var x, y int
	harness.onUI(func() { x, y, _, _ = harness.ui.suggestions.GetInnerRect() })
	harness.click(x+1, y+1)
	harness.wait(func() bool { return harness.ui.current != nil && harness.ui.current.Query == "car" })
	harness.onUI(func() { x, y, _, _ = harness.ui.results.GetInnerRect() })
	harness.ui.app.QueueEvent(tcell.NewEventMouse(x+2, y+2, tcell.WheelDown, tcell.ModNone))
	harness.wait(func() bool { row, _ := harness.ui.results.GetScrollOffset(); return row > 0 })
	harness.resize(60, 24)
	harness.onUI(func() {
		_, _, width, _ := harness.ui.search.GetRect()
		_, top, _, _ := harness.ui.results.GetRect()
		if width != 60 || !harness.ui.narrow || top < 9 {
			t.Errorf("narrow layout: width=%d top=%d", width, top)
		}
	})
	harness.key(tcell.KeyRune, 'G')
	harness.wait(func() bool { row, _ := harness.ui.results.GetScrollOffset(); return row > 50 })
}

func TestPronunciationButtonsInsideDictionary(t *testing.T) {
	for _, glyphs := range []string{"plain", "nerd", "emoji"} {
		t.Run(glyphs, func(t *testing.T) {
			engine := &engineStub{lookup: func(_ context.Context, query string) (dictionary.LookupResult, error) {
				return dictionary.LookupResult{Query: query, Primary: &dictionary.Entry{Word: query, Phonetic: "kat", PhoneticUS: "kaet"}}, nil
			}}
			harness := startUI(t, engine, func(ui *UI) {
				ui.config.Glyphs = glyphs
				ui.build()
			})
			harness.query("cat")
			harness.wait(func() bool { return harness.ui.pronunciationAvailable == [2]bool{true, true} })
			for _, size := range [][2]int{{110, 34}, {60, 20}} {
				harness.resize(size[0], size[1])
				for _, button := range []*tview.Button{harness.ui.british, harness.ui.american} {
					var left, top int
					harness.onUI(func() {
						innerLeft, innerTop, width, height := harness.ui.dictionaryPane.GetInnerRect()
						var buttonWidth, buttonHeight int
						left, top, buttonWidth, buttonHeight = button.GetRect()
						phonetic := "UK  /kat/"
						if button == harness.ui.american {
							phonetic = "US  /kaet/"
						}
						var line strings.Builder
						for column := innerLeft; column < innerLeft+width; column++ {
							character, _, _, _ := harness.screen.GetContent(column, top)
							line.WriteRune(character)
						}
						if !strings.Contains(line.String(), phonetic) {
							t.Errorf("%dx%d: button does not share its phonetic row: %q", size[0], size[1], line.String())
						}
						if left < innerLeft || top < innerTop || left+buttonWidth > innerLeft+width || top+buttonHeight > innerTop+height {
							t.Errorf("%dx%d: %s button outside dictionary", size[0], size[1], button.GetLabel())
						}
						_, wordTop, _, wordHeight := harness.ui.headword.GetRect()
						_, resultsTop, _, _ := harness.ui.results.GetRect()
						if top < wordTop+wordHeight || top+buttonHeight > resultsTop {
							t.Errorf("%dx%d: pronunciation not between headword and definition", size[0], size[1])
						}
						if harness.ui.headword.GetText(true) != "cat" {
							t.Error("headword missing from dictionary header")
						}
					})
					harness.click(left+1, top)
					harness.wait(func() bool {
						harness.speaker.gate.Lock()
						defer harness.speaker.gate.Unlock()
						return len(harness.speaker.words) > 0
					})
					harness.onUI(func() {
						harness.speaker.gate.Lock()
						defer harness.speaker.gate.Unlock()
						accent := dictionary.British
						if button == harness.ui.american {
							accent = dictionary.American
						}
						if got := harness.speaker.words[0]; got != "cat:"+string(accent) {
							t.Errorf("wrong pronunciation: %s", got)
						}
						harness.speaker.words = nil
					})
				}
			}
		})
	}
}

type audioBackendStub struct {
	Backend
	err   error
	audio func(context.Context, string, dictionary.Accent) ([]byte, error)
}

func (backend audioBackendStub) Audio(ctx context.Context, word string, accent dictionary.Accent) ([]byte, error) {
	if backend.audio != nil {
		return backend.audio(ctx, word, accent)
	}
	return []byte("audio"), backend.err
}

func TestPronunciationAvailability(t *testing.T) {
	for _, test := range []struct {
		name      string
		available [2]bool
		err       error
	}{
		{name: "both", available: [2]bool{true, true}},
		{name: "british", available: [2]bool{true, false}},
		{name: "american", available: [2]bool{false, true}},
		{name: "missing", err: dictionary.ErrAudioNotFound},
		{name: "empty"},
		{name: "unreadable", err: errors.New("broken audio database")},
	} {
		t.Run(test.name, func(t *testing.T) {
			engine := &engineStub{lookup: func(_ context.Context, query string) (dictionary.LookupResult, error) {
				if query == "miss" {
					return dictionary.LookupResult{Query: query}, nil
				}
				return dictionary.LookupResult{Query: query, Primary: &dictionary.Entry{Word: "cat", Phonetic: "kat", PhoneticUS: "kaet"}}, nil
			}}
			harness := startUI(t, engine, func(ui *UI) {
				ui.backend = audioBackendStub{Backend: ui.backend, audio: func(_ context.Context, word string, accent dictionary.Accent) ([]byte, error) {
					if word != "cat" {
						t.Errorf("audio used query instead of headword: %q", word)
					}
					index := 0
					if accent == dictionary.American {
						index = 1
					}
					if test.available[index] {
						return []byte("audio"), nil
					}
					return nil, test.err
				}}
			})
			harness.query("cats")
			harness.wait(func() bool { return harness.ui.pronunciationCancel == nil })
			harness.onUI(func() {
				if harness.ui.pronunciationAvailable != test.available {
					t.Errorf("availability: got %v, want %v", harness.ui.pronunciationAvailable, test.available)
				}
				for index, button := range []*tview.Button{harness.ui.british, harness.ui.american} {
					row := harness.ui.pronunciation.GetItem(index).(*tview.Flex)
					if row.GetItemCount() == 2 != test.available[index] {
						t.Errorf("accent %d: unexpected button visibility", index)
					}
					if text := row.GetItem(0).(*tview.TextView).GetText(true); !strings.Contains(text, "/") {
						t.Errorf("missing audio removed phonetic: %q", text)
					}
					if !test.available[index] {
						_, _, width, height := button.GetRect()
						if width != 0 || height != 0 {
							t.Error("hidden button retains screen area")
						}
					}
				}
				harness.ui.app.SetFocus(harness.ui.results)
			})
			for index, button := range []*tview.Button{harness.ui.british, harness.ui.american} {
				if test.available[index] {
					harness.key(tcell.KeyTab, 0)
					harness.wait(button.HasFocus)
				}
			}
			harness.key(tcell.KeyTab, 0)
			harness.wait(harness.ui.links.HasFocus)
			harness.query("miss")
			harness.onUI(func() {
				if harness.ui.pronunciation.GetItemCount() != 0 || harness.ui.pronunciationAvailable != [2]bool{} {
					t.Error("missing entry retained pronunciation controls")
				}
			})
		})
	}
}

func TestPronunciationRefreshAfterAudioDownload(t *testing.T) {
	ready := make(chan struct{})
	harness := startUI(t, &engineStub{}, func(ui *UI) {
		ui.backend = audioBackendStub{Backend: ui.backend, audio: func(context.Context, string, dictionary.Accent) ([]byte, error) {
			select {
			case <-ready:
				return []byte("audio"), nil
			default:
				return nil, dictionary.ErrAudioNotFound
			}
		}}
	})
	harness.query("cat")
	harness.wait(func() bool { return harness.ui.pronunciationCancel == nil })
	harness.onUI(func() {
		if harness.ui.pronunciation.GetItemCount() != 0 {
			t.Error("empty phonetics and missing audio left empty rows")
		}
		harness.ui.SetInitializer(func(_ context.Context, _ bool, report assets.Reporter) error {
			close(ready)
			report(assets.Progress{Asset: assets.Audio, Phase: "ready"})
			return nil
		}, false, true)
		harness.ui.initialize(false)
	})
	harness.wait(func() bool { return harness.ui.pronunciationAvailable == [2]bool{true, true} })
}

func TestPronunciationErrorsAndFocus(t *testing.T) {
	harness := startUI(t, &engineStub{})
	harness.onUI(func() { harness.ui.app.SetFocus(harness.ui.results) })
	harness.key(tcell.KeyTab, 0)
	harness.wait(func() bool { return harness.ui.links.HasFocus() })
	harness.query("cat")
	harness.wait(func() bool { return harness.ui.pronunciationAvailable == [2]bool{true, true} })
	harness.key(tcell.KeyTab, 0)
	harness.wait(func() bool { return harness.ui.british.HasFocus() })
	harness.key(tcell.KeyTab, 0)
	harness.wait(func() bool { return harness.ui.american.HasFocus() })
	for _, test := range []struct {
		err     error
		message string
	}{
		{errors.New("broken audio database"), "broken audio database"},
		{dictionary.ErrAudioNotFound, "install or update the audio pack"},
	} {
		harness.onUI(func() {
			harness.ui.backend = audioBackendStub{Backend: harness.service, err: test.err}
		})
		harness.key(tcell.KeyRune, 'u')
		harness.wait(func() bool { return strings.Contains(harness.ui.status.GetText(false), test.message) })
	}
	harness.speaker.gate.Lock()
	defer harness.speaker.gate.Unlock()
	if len(harness.speaker.words) != 0 {
		t.Fatal("audio errors started playback")
	}
}

func TestHistoryEditingAndClearConfirmation(t *testing.T) {
	harness := startUI(t, &engineStub{})
	harness.query("cat")
	harness.wait(func() bool { return len(harness.ui.records) == 1 })
	harness.key(tcell.KeyF2, 0)
	harness.wait(func() bool { return harness.ui.recents.HasFocus() })
	harness.key(tcell.KeyRune, 'e')
	harness.wait(func() bool { return harness.ui.dialog })
	harness.onUI(func() { harness.ui.dialogBody.(*tview.Form).GetFormItem(0).(*tview.InputField).SetText("kitten") })
	harness.key(tcell.KeyTab, 0)
	harness.key(tcell.KeyEnter, 0)
	harness.wait(func() bool {
		return !harness.ui.dialog && len(harness.ui.records) == 1 && harness.ui.records[0].Query == "kitten"
	})
	harness.key(tcell.KeyRune, 'D')
	harness.wait(func() bool { return harness.ui.dialog })
	harness.key(tcell.KeyEnter, 0) // Default is Cancel, never the destructive button.
	harness.wait(func() bool { return !harness.ui.dialog })
	harness.onUI(func() {
		if len(harness.ui.records) != 1 {
			t.Error("cancel cleared history")
		}
	})
	harness.key(tcell.KeyRune, 'D')
	harness.wait(func() bool { return harness.ui.dialog })
	harness.key(tcell.KeyTab, 0)
	harness.key(tcell.KeyEnter, 0)
	harness.wait(func() bool { return !harness.ui.dialog && len(harness.ui.records) == 0 })
}

func TestSettingsAboutAndPlayback(t *testing.T) {
	harness := startUI(t, &engineStub{})
	harness.query("cat")
	harness.key(tcell.KeyRune, 'u')
	harness.wait(func() bool {
		harness.speaker.gate.Lock()
		defer harness.speaker.gate.Unlock()
		return len(harness.speaker.words) == 1
	})
	harness.key(tcell.KeyF3, 0)
	harness.wait(func() bool { return harness.ui.dialog })
	harness.onUI(func() {
		form := harness.ui.dialogBody.(*tview.Form)
		form.GetFormItem(0).(*tview.DropDown).SetCurrentOption(1)
		form.GetFormItem(1).(*tview.DropDown).SetCurrentOption(1)
		form.GetFormItem(4).(*tview.DropDown).SetCurrentOption(1)
		harness.ui.app.SetFocus(form.GetButton(0))
	})
	harness.key(tcell.KeyEnter, 0)
	harness.wait(func() bool {
		return harness.ui.config.Theme == "light" && harness.ui.config.Language == "zh" && harness.ui.config.Glyphs == "nerd" && !harness.ui.configuring
	})
	config, err := profile.Load(harness.ui.configPath)
	if err != nil || config.Theme != "light" || config.Language != "zh" || config.Glyphs != "nerd" {
		t.Fatalf("settings persistence: %+v %v", config, err)
	}
	harness.onUI(func() {
		if !strings.Contains(harness.ui.british.GetLabel(), "\uf028") || harness.ui.headword.GetText(true) != "cat" {
			t.Error("icon setting did not rebuild the current result")
		}
	})
	harness.onUI(func() {
		harness.ui.version = "v1.2.3\nCommit: abc1234\nBuilt: 2026-10-11T12:00:00Z"
	})
	harness.key(tcell.KeyF4, 0)
	harness.wait(func() bool { return harness.ui.dialog })
	harness.onUI(func() {
		panel := harness.ui.dialogBody.(*tview.Flex)
		text := panel.GetItem(0).(*tview.TextView).GetText(true)
		text = strings.Join(strings.Fields(text), " ")
		for _, expected := range []string{"https://github.com/mingcheng/stupidict", "v1.2.3", "Commit abc1234", "Built 2026-10-11T12:00:00Z"} {
			if !strings.Contains(text, expected) {
				t.Errorf("about dialog missing %q: %s", expected, text)
			}
		}
	})
	harness.key(tcell.KeyEsc, 0)
	harness.wait(func() bool { return !harness.ui.dialog })
}

func TestGlyphModeSwitch(t *testing.T) {
	harness := startUI(t, &engineStub{})
	harness.query("cat")
	for _, mode := range []struct {
		index int
		name  string
		label string
	}{
		{1, "nerd", "\uf028 UK"},
		{2, "emoji", "\U0001f50a UK"},
		{0, "plain", "Play UK"},
	} {
		harness.key(tcell.KeyF3, 0)
		harness.wait(func() bool { return harness.ui.dialog })
		harness.onUI(func() {
			form := harness.ui.dialogBody.(*tview.Form)
			form.GetFormItem(4).(*tview.DropDown).SetCurrentOption(mode.index)
			harness.ui.app.SetFocus(form.GetButton(0))
		})
		harness.key(tcell.KeyEnter, 0)
		harness.wait(func() bool { return harness.ui.config.Glyphs == mode.name && !harness.ui.configuring })
		harness.onUI(func() {
			if harness.ui.british.GetLabel() != mode.label || harness.ui.headword.GetText(true) != "cat" {
				t.Errorf("%s: switching icons lost the current result or speaker glyph", mode.name)
			}
		})
		config, err := profile.Load(harness.ui.configPath)
		if err != nil || config.Glyphs != mode.name {
			t.Fatalf("%s: saved glyph mode: %+v, %v", mode.name, config, err)
		}
	}
}

func TestRightAlignedQuitButton(t *testing.T) {
	harness := startUI(t, &engineStub{}, func(ui *UI) {
		ui.config.Glyphs = "emoji"
		ui.build()
	})
	harness.resize(60, 20)
	var left, top, width int
	harness.onUI(func() { left, top, width, _ = harness.ui.quit.GetRect() })
	harness.click(left+width/2, top)
	select {
	case <-harness.ui.ctx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("right-aligned Quit button did not exit")
	}
}

func TestEmojiDownloadIcons(t *testing.T) {
	harness := startUI(t, &engineStub{}, func(ui *UI) {
		ui.config.Glyphs = "emoji"
		ui.build()
	})
	harness.resize(60, 20)
	for _, state := range []struct {
		downloading bool
		err         error
		label       string
	}{
		{false, nil, ""},
		{true, nil, "\U0001f6d1 Cancel"},
		{false, errors.New("download failed"), "\U0001f504 Retry"},
	} {
		harness.onUI(func() {
			harness.ui.downloading, harness.ui.downloadError = state.downloading, state.err
			harness.ui.renderDownload()
		})
		harness.onUI(func() {
			if state.label == "" {
				if harness.ui.footer.GetItemCount() != 1 || harness.ui.footer.GetItem(0) != harness.ui.status {
					t.Error("data controls visible outside initialization")
				}
				return
			}
			button := harness.ui.downloadButton
			left, top, width, _ := button.GetRect()
			_, footerTop, _, _ := harness.ui.footer.GetRect()
			if button.GetLabel() != state.label || left+width > 60 || top < footerTop || tview.TaggedStringWidth(state.label) > width {
				t.Errorf("download icon missing or clipped: %q at %d, width %d", button.GetLabel(), left, width)
			}
		})
	}
}

func TestLookupErrorsAndEscapedContent(t *testing.T) {
	harness := startUI(t, &engineStub{lookup: func(context.Context, string) (dictionary.LookupResult, error) {
		return dictionary.LookupResult{}, errors.New("broken database")
	}})
	harness.onUI(func() { harness.ui.lookup("cat") })
	harness.wait(func() bool { return strings.Contains(harness.ui.status.GetText(false), "broken database") })
	harness.onUI(func() {
		if harness.ui.current != nil || len(harness.ui.records) != 0 {
			t.Error("failed query accepted")
		}
		harness.ui.render(dictionary.LookupResult{Query: "[red]", Primary: &dictionary.Entry{Word: "[red]", Translation: "中文 [blue] literal"}})
		if !strings.Contains(harness.ui.results.GetText(true), "[blue]") {
			t.Error("dictionary text interpreted as markup")
		}
	})
}

type infoBackendStub struct {
	Backend
	info func(context.Context) client.DataInfo
}

func (backend infoBackendStub) DataInfo(ctx context.Context) client.DataInfo {
	return backend.info(ctx)
}

func TestAboutDataInfoAndGitHub(t *testing.T) {
	for _, language := range []string{"en", "zh"} {
		for _, failed := range []bool{false, true} {
			name := language + "/success"
			if failed {
				name = language + "/failure"
			}
			t.Run(name, func(t *testing.T) {
				opened := make(chan string, 1)
				modified := time.Date(2024, 6, 12, 10, 30, 0, 0, time.UTC)
				harness := startUI(t, &engineStub{}, func(ui *UI) {
					ui.config.Language = language
					ui.backend = infoBackendStub{Backend: ui.backend, info: func(context.Context) client.DataInfo {
						return client.DataInfo{Entries: 314159,
							Dictionary: client.FileInfo{Size: 2097152, Modified: modified},
							Audio:      client.FileInfo{Err: os.ErrNotExist}}
					}}
					ui.openURL = func(_ context.Context, url string) error {
						opened <- url
						if failed {
							return errors.New("browser unavailable")
						}
						return nil
					}
				})
				harness.resize(60, 20)
				harness.key(tcell.KeyF4, 0)
				harness.wait(func() bool {
					if !harness.ui.dialog {
						return false
					}
					text := harness.ui.dialogBody.(*tview.Flex).GetItem(0).(*tview.TextView).GetText(true)
					return strings.Contains(text, "314159")
				})
				var left, top int
				harness.onUI(func() {
					panel := harness.ui.dialogBody.(*tview.Flex)
					text := panel.GetItem(0).(*tview.TextView).GetText(true)
					compact := strings.Join(strings.Fields(text), "")
					for _, expected := range []string{"2097152", modified.Local().Format(time.RFC3339),
						harness.ui.tr("Not installed", "未安装"), harness.ui.tr("not data publication dates", "并非数据发布日期")} {
						if !strings.Contains(compact, strings.Join(strings.Fields(expected), "")) {
							t.Errorf("about missing %q: %s", expected, text)
						}
					}
					button := panel.GetItem(1).(*tview.Form).GetButton(1)
					var width, height int
					left, top, width, height = button.GetRect()
					if left < 0 || top < 0 || left+width > 60 || top+height > 20 {
						t.Error("GitHub button outside small viewport")
					}
				})
				harness.click(left+1, top)
				select {
				case url := <-opened:
					if url != projectURL {
						t.Fatalf("opened URL %q", url)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("GitHub click did not invoke opener")
				}
				harness.wait(func() bool {
					status := harness.ui.status.GetText(true)
					if failed {
						return strings.Contains(status, projectURL) && strings.Contains(status, "browser unavailable")
					}
					return status == harness.ui.tr("Link sent to your default browser", "链接已交给默认浏览器")
				})
				harness.key(tcell.KeyEsc, 0)
				harness.wait(func() bool { return !harness.ui.dialog })
			})
		}
	}
}

func TestAboutCancelAndIgnoreLateData(t *testing.T) {
	started, canceled := make(chan struct{}), make(chan struct{})
	harness := startUI(t, &engineStub{}, func(ui *UI) {
		ui.backend = infoBackendStub{Backend: ui.backend, info: func(ctx context.Context) client.DataInfo {
			close(started)
			<-ctx.Done()
			close(canceled)
			return client.DataInfo{Entries: 987654321}
		}}
	})
	harness.key(tcell.KeyF4, 0)
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("statistics did not start")
	}
	harness.wait(func() bool {
		text := harness.ui.dialogBody.(*tview.Flex).GetItem(0).(*tview.TextView).GetText(true)
		return strings.Contains(text, "Loading...")
	})
	harness.onUI(func() {
		harness.ui.closeDialog()
		harness.ui.help()
	})
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("closing About did not cancel statistics")
	}
	harness.onUI(func() {
		text := harness.ui.dialogBody.(*tview.Flex).GetItem(0).(*tview.TextView).GetText(true)
		if strings.Contains(text, "987654321") || !strings.Contains(text, "Search and navigation") {
			t.Fatal("late About result replaced Help")
		}
	})
}

func TestAboutBrowserKeyboardAndCancellation(t *testing.T) {
	started, canceled := make(chan struct{}), make(chan struct{})
	harness := startUI(t, &engineStub{}, func(ui *UI) {
		ui.openURL = func(ctx context.Context, url string) error {
			close(started)
			<-ctx.Done()
			close(canceled)
			return ctx.Err()
		}
	})
	harness.key(tcell.KeyF4, 0)
	harness.wait(func() bool { return harness.ui.dialog })
	harness.key(tcell.KeyTab, 0)
	harness.key(tcell.KeyBacktab, 0)
	harness.wait(func() bool {
		return harness.ui.dialogBody.(*tview.Flex).GetItem(0).HasFocus()
	})
	harness.key(tcell.KeyTab, 0)
	harness.key(tcell.KeyTab, 0)
	harness.key(tcell.KeyEnter, 0)
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("keyboard did not activate GitHub")
	}
	harness.key(tcell.KeyEsc, 0)
	harness.wait(func() bool { return !harness.ui.dialog })
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("closing About did not cancel browser opener")
	}
}

func TestDialogTextAlignmentAndWrapping(t *testing.T) {
	rows := [][2]string{
		{"", "Section"},
		{"Key", "First"},
		{"Config file", "Second"},
		{"配置文件", "Third"},
		{"", ""},
		{"Long value", strings.Repeat("directory/", 12) + "[red]配置.json"},
	}
	for _, width := range []int{8, 56, 74} {
		text := dialogText(rows, width)
		for _, line := range strings.Split(text, "\n") {
			if actual := tview.TaggedStringWidth(line); actual > width {
				t.Errorf("width %d: line width %d: %q", width, actual, line)
			}
		}
		if width < 12 {
			continue
		}
		column := -1
		for _, line := range strings.Split(text, "\n") {
			for _, value := range []string{"First", "Second", "Third"} {
				if index := strings.Index(line, value); index >= 0 {
					actual := tview.TaggedStringWidth(line[:index])
					if column >= 0 && actual != column {
						t.Errorf("width %d: description column %d, want %d", width, actual, column)
					}
					column = actual
				}
			}
		}
		if strings.Contains(text, "\n\n\n") {
			t.Error("section spacing contains duplicate blank lines")
		}
	}
}

func TestInformationDialogsResizeAndContent(t *testing.T) {
	for _, language := range []string{"en", "zh"} {
		t.Run(language, func(t *testing.T) {
			harness := startUI(t, &engineStub{}, func(ui *UI) {
				ui.config.Language = language
				ui.config.Dictionary = "/tmp/" + strings.Repeat("long-directory/", 8) + "[red]dictionary.db"
				ui.config.Audio = ""
			})
			for _, key := range []tcell.Key{tcell.KeyF1, tcell.KeyF4} {
				harness.key(key, 0)
				harness.wait(func() bool { return harness.ui.dialog })
				for _, size := range [][2]int{{110, 34}, {60, 20}, {80, 24}} {
					harness.resize(size[0], size[1])
					harness.onUI(func() {
						panel := harness.ui.dialogBody.(*tview.Flex)
						left, top, width, height := panel.GetRect()
						if left < 0 || top < 0 || left+width > size[0] || top+height > size[1] {
							t.Errorf("dialog outside %dx%d viewport", size[0], size[1])
						}
						text := panel.GetItem(0).(*tview.TextView)
						_, _, innerWidth, _ := text.GetInnerRect()
						for _, line := range strings.Split(text.GetText(false), "\n") {
							if tview.TaggedStringWidth(line) > innerWidth {
								t.Errorf("line exceeds content width %d: %q", innerWidth, line)
							}
						}
						content := strings.Join(strings.Fields(text.GetText(true)), "")
						expected := []string{"Ctrl+K", "Shift+Tab", "F5", "-update-data", "-download", "-config"}
						if key == tcell.KeyF4 {
							expected = []string{harness.ui.configPath, "history.db", harness.ui.config.Dictionary,
								harness.ui.tr("Not configured", "未配置"), "data-3", harness.ui.tr("system audio output", "系统音频输出")}
						}
						for _, value := range expected {
							if !strings.Contains(content, strings.Join(strings.Fields(value), "")) {
								t.Errorf("dialog missing %q", value)
							}
						}
						button := panel.GetItem(1).(*tview.Form).GetButton(0)
						buttonLeft, buttonTop, buttonWidth, buttonHeight := button.GetRect()
						if buttonLeft < left || buttonTop < top || buttonLeft+buttonWidth > left+width || buttonTop+buttonHeight > top+height {
							t.Error("close button outside dialog")
						}
					})
				}
				harness.key(tcell.KeyEnd, 0)
				harness.wait(func() bool {
					text := harness.ui.dialogBody.(*tview.Flex).GetItem(0).(*tview.TextView)
					row, _ := text.GetScrollOffset()
					return row > 0
				})
				harness.key(tcell.KeyTab, 0)
				harness.key(tcell.KeyEnter, 0)
				harness.wait(func() bool { return !harness.ui.dialog })
				harness.onUI(func() {
					if !harness.ui.search.HasFocus() {
						t.Error("closing dialog did not restore search focus")
					}
				})
			}
		})
	}
}

func TestSmallDialogsScrollAndBlockBackgroundMouse(t *testing.T) {
	harness := startUI(t, &engineStub{})
	harness.query("cat")
	harness.resize(60, 20)
	harness.key(tcell.KeyF1, 0)
	harness.wait(func() bool { return harness.ui.dialog })
	harness.onUI(func() {
		left, top, width, height := harness.ui.dialogBody.GetRect()
		if left < 0 || top < 0 || left+width > 60 || top+height > 20 {
			t.Errorf("dialog outside viewport: %d,%d %dx%d", left, top, width, height)
		}
	})
	harness.click(7, 1) // History button behind the help dialog.
	harness.onUI(func() {
		if harness.ui.historyVisible {
			t.Error("modal allowed background click")
		}
	})
	harness.key(tcell.KeyRune, 'G')
	harness.wait(func() bool {
		text := harness.ui.dialogBody.(*tview.Flex).GetItem(0).(*tview.TextView)
		row, _ := text.GetScrollOffset()
		return row > 0
	})
	harness.key(tcell.KeyTab, 0)
	harness.key(tcell.KeyEnter, 0)
	harness.wait(func() bool { return !harness.ui.dialog })
}

func TestStaleCompletionDoesNotReplaceNewInput(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	harness := startUI(t, &engineStub{suggest: func(_ context.Context, query string) ([]string, error) {
		if query == "old" {
			close(started)
			<-release
		}
		return []string{query + "-candidate"}, nil
	}})
	harness.onUI(func() { harness.ui.search.SetText("old") })
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("completion did not start")
	}
	harness.onUI(func() { harness.ui.search.SetText("new") })
	harness.wait(func() bool {
		if harness.ui.suggestions.GetItemCount() == 0 {
			return false
		}
		text, _ := harness.ui.suggestions.GetItemText(0)
		return text == "new-candidate"
	})
	close(release)
	harness.onUI(func() {
		text, _ := harness.ui.suggestions.GetItemText(0)
		if text != "new-candidate" || harness.ui.search.GetText() != "new" {
			t.Error("stale completion applied")
		}
	})
}

func TestRealDictionaryTUI(t *testing.T) {
	path := os.Getenv("STUPIDDICT_DICTIONARY")
	if path == "" {
		t.Skip("STUPIDDICT_DICTIONARY not set")
	}
	engine, err := dictionary.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	harness := startUI(t, &engineStub{lookup: engine.Lookup, suggest: engine.Suggest})
	harness.query("cat")
	harness.onUI(func() {
		if harness.ui.current.Primary == nil || harness.ui.current.Primary.Word != "cat" || harness.ui.links.GetItemCount() == 0 {
			t.Error("real entry / thesaurus missing")
		}
	})
	harness.query("dictionray")
	harness.onUI(func() {
		if harness.ui.current.Primary != nil || harness.ui.current.Tier != dictionary.Fuzzy {
			t.Error("fuzzy query was auto-corrected")
		}
	})
	harness.query("猫")
	harness.onUI(func() {
		if len(harness.ui.current.ChineseMatches) == 0 {
			t.Error("Chinese reverse lookup missing")
		}
	})
}

func TestShutdownDrainsOrderedHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	service, err := client.Open(context.Background(), path, profile.Defaults(), func(context.Context, string) (client.Engine, error) { return &engineStub{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	ui := New(service, &speakerStub{}, path, "test")
	ui.SetScreen(tcell.NewSimulationScreen("UTF-8"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- ui.Run(ctx) }()
	select {
	case <-ui.ready:
	case <-time.After(5 * time.Second):
		t.Fatal("UI did not start")
	}
	completed := 0
	ui.updates <- func() {
		for range 512 {
			ui.changeHistory(func() error {
				completed++
				return nil
			})
		}
		for _, query := range []string{"cat", "dog"} {
			ui.changeHistory(func() error {
				return service.Accept(dictionary.LookupResult{Query: query, Primary: &dictionary.Entry{Word: query}})
			})
			if query == "cat" {
				ui.changeHistory(service.ClearHistory)
			}
		}
		cancel()
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("UI shutdown stalled")
	}
	if completed != 512 {
		t.Fatalf("lost history operations: completed %d of 512", completed)
	}
	records, err := service.History()
	if err != nil || len(records) != 1 || records[0].Query != "dog" {
		t.Fatalf("lost or reordered history: %+v %v", records, err)
	}
}

func TestRenderedCellsAtSupportedSizes(t *testing.T) {
	for _, glyphs := range []string{"plain", "nerd", "emoji"} {
		for _, language := range []string{"en", "zh"} {
			t.Run(glyphs+"/"+language, func(t *testing.T) {
				harness := startUI(t, &engineStub{}, func(ui *UI) {
					ui.config.Glyphs, ui.config.Language = glyphs, language
					ui.build()
				})
				harness.query("cat")
				for _, size := range [][2]int{{110, 34}, {80, 24}, {60, 20}} {
					harness.resize(size[0], size[1])
					harness.onUI(func() {
						var display strings.Builder
						for row := 0; row < size[1]; row++ {
							for column := 0; column < size[0]; {
								character, combining, _, width := harness.screen.GetContent(column, row)
								display.WriteRune(character)
								for _, mark := range combining {
									display.WriteRune(mark)
								}
								column += max(1, width)
							}
							display.WriteByte('\n')
						}
						labels := []string{"stupidict", "cat", "definition", "中文", "UK", "US",
							harness.ui.tr("Dictionary", "词典"), harness.ui.tr("History", "历史"),
							harness.ui.tr("Settings", "设置"), harness.ui.tr("About", "关于"),
							harness.ui.tr("Quit", "退出")}
						if glyphs == "nerd" {
							labels = append(labels, "\uf028", "\uf002", "\uf013", "\uf02d")
						} else if glyphs == "emoji" {
							labels = append(labels, "\U0001f50a", "\U0001f50d", "\U0001f527", "\U0001f4d6", "\u2b05\ufe0f", "\u27a1\ufe0f", "\u2753")
						}
						if glyphs != "nerd" && strings.Contains(display.String(), "\uf028") {
							t.Error("Nerd Font icon leaked into a portable mode")
						}
						for _, expected := range labels {
							if !strings.Contains(display.String(), expected) {
								t.Errorf("%dx%d: missing rendered %q\n%s", size[0], size[1], expected, display.String())
							}
						}
						if strings.Contains(display.String(), harness.ui.tr("Data", "数据")) {
							t.Error("idle interface still shows Data")
						}
						quitLeft, quitTop, quitWidth, _ := harness.ui.quit.GetRect()
						if quitLeft+quitWidth != size[0] || quitTop != 1 {
							t.Errorf("%dx%d: Quit not right-aligned in navigation bar", size[0], size[1])
						}
						for _, button := range []*tview.Button{harness.ui.british, harness.ui.american, harness.ui.quit} {
							left, top, width, height := button.GetRect()
							if left < 0 || top < 0 || left+width > size[0] || top+height > size[1] || tview.TaggedStringWidth(button.GetLabel()) > width {
								t.Errorf("%dx%d: clipped button %q", size[0], size[1], button.GetLabel())
							}
						}
						_, _, style, _ := harness.screen.GetContent(0, 0)
						foreground, _, _ := style.Decompose()
						if foreground != harness.ui.palette.accent {
							t.Error("title color not rendered")
						}
					})
				}
			})
		}
	}
}

func TestInitializationProgressCancellationAndRetry(t *testing.T) {
	ready := make(chan struct{})
	attempts := 0
	harness := startUI(t, &engineStub{}, func(ui *UI) {
		ui.SetInitializer(func(ctx context.Context, force bool, report assets.Reporter) error {
			attempts++
			if !force {
				t.Error("forced retry lost update intent")
			}
			if attempts == 1 {
				report(assets.Progress{Asset: assets.Dictionary, Phase: "download", Received: 50, Total: 100})
				select {
				case <-ready:
				case <-ctx.Done():
					return ctx.Err()
				}
				report(assets.Progress{Asset: assets.Dictionary, Phase: "ready"})
				report(assets.Progress{Asset: assets.Audio, Phase: "download", Received: 50, Total: 100})
				<-ctx.Done()
				return ctx.Err()
			}
			report(assets.Progress{Asset: assets.Dictionary, Phase: "ready"})
			report(assets.Progress{Asset: assets.Audio, Phase: "ready"})
			return nil
		}, true, false)
	})
	harness.wait(func() bool { return strings.Contains(harness.ui.downloadInfo.GetText(false), "50%") })
	harness.onUI(func() {
		harness.ui.lookup("blocked")
		if harness.ui.current != nil || !harness.ui.dictionaryPending {
			t.Error("queried before dictionary ready")
		}
	})
	close(ready)
	harness.wait(func() bool {
		return !harness.ui.dictionaryPending && strings.Contains(harness.ui.downloadInfo.GetText(false), assets.Audio)
	})
	harness.query("cat")
	harness.onUI(func() {
		if !harness.ui.downloading {
			t.Error("query waited for audio download")
		}
	})
	var cancelLeft, cancelTop int
	harness.onUI(func() { cancelLeft, cancelTop, _, _ = harness.ui.downloadButton.GetRect() })
	harness.click(cancelLeft+1, cancelTop)
	harness.wait(func() bool { return !harness.ui.downloading && errors.Is(harness.ui.downloadError, context.Canceled) })
	harness.key(tcell.KeyF5, 0)
	harness.wait(func() bool { return !harness.ui.downloading && harness.ui.downloadError == nil })
	harness.key(tcell.KeyF5, 0)
	harness.key(tcell.KeyF4, 0)
	harness.wait(func() bool { return harness.ui.dialog })
	harness.onUI(func() {
		ui := harness.ui
		ui.toggleDownload()
		if ui.downloading || ui.footer.GetItemCount() != 1 || ui.downloadButton.HasFocus() {
			t.Error("completed initialization can still be restarted from the UI")
		}
	})
}
