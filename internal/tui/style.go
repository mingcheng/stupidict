package tui

import (
	"os"
	"strings"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

type palette struct{ background, surface, text, muted, accent, border tcell.Color }

func colors(theme string) palette {
	switch theme {
	case "light":
		return palette{tcell.GetColor("#f5f7f8"), tcell.GetColor("#e8edef"), tcell.GetColor("#20262b"), tcell.GetColor("#637178"), tcell.GetColor("#006b59"), tcell.GetColor("#9cabb1")}
	case "dark":
		return palette{tcell.GetColor("#181c1e"), tcell.GetColor("#242b2e"), tcell.GetColor("#edf3f4"), tcell.GetColor("#a0aeb3"), tcell.GetColor("#62dbb0"), tcell.GetColor("#53636c")}
	default:
		return palette{tcell.ColorDefault, tcell.ColorDefault, tcell.ColorDefault, tcell.ColorGray, tcell.ColorTeal, tcell.ColorGray}
	}
}

func (ui *UI) tr(english, chinese string) string {
	language := ui.config.Language
	if language == "system" {
		locale := os.Getenv("LC_ALL")
		if locale == "" {
			locale = os.Getenv("LC_MESSAGES")
		}
		if locale == "" {
			locale = os.Getenv("LANG")
		}
		if strings.HasPrefix(strings.ToLower(locale), "zh") {
			language = "zh"
		}
	}
	if language == "zh" {
		return chinese
	}
	return english
}

func (ui *UI) iconGlyph(nerd string) string {
	switch ui.config.Glyphs {
	case "nerd":
		return nerd
	case "emoji":
		switch nerd {
		case "\uf002":
			return "\U0001f50d"
		case "\uf0ca":
			return "\U0001f4cb"
		case "\uf1da":
			return "\U0001f552"
		case "\uf0c1":
			return "\U0001f517"
		case "\uf02d":
			return "\U0001f4d6"
		case "\uf013":
			return "\U0001f527"
		case "\uf05a":
			return "\U0001f4a1"
		case "\uf08b":
			return "\U0001f6aa"
		case "\uf019":
			return "\U0001f4e5"
		case "\uf04d":
			return "\U0001f6d1"
		case "\uf01e":
			return "\U0001f504"
		case "\uf060":
			return "\u2b05\ufe0f"
		case "\uf061":
			return "\u27a1\ufe0f"
		case "\uf059":
			return "\u2753"
		case "\uf028":
			return "\U0001f50a"
		}
	}
	return ""
}

func (ui *UI) iconLabel(icon, label string) string {
	if glyph := ui.iconGlyph(icon); glyph != "" {
		return glyph + " " + label
	}
	return label
}

func (ui *UI) box(box *tview.Box, title string) {
	box.SetBorder(true).SetTitle(title).SetTitleAlign(tview.AlignLeft).
		SetBorderColor(ui.palette.border).SetTitleColor(ui.palette.accent).SetBackgroundColor(ui.palette.background)
}

func (ui *UI) textView() *tview.TextView {
	view := tview.NewTextView().SetTextColor(ui.palette.text).SetWrap(true).SetWordWrap(true)
	view.SetBackgroundColor(ui.palette.background)
	return view
}

func (ui *UI) list() *tview.List {
	list := tview.NewList().SetMainTextColor(ui.palette.text).SetSecondaryTextColor(ui.palette.muted).
		SetSelectedStyle(tcell.StyleDefault.Background(ui.palette.accent).Foreground(ui.palette.background)).
		SetHighlightFullLine(true).SetSelectedFocusOnly(true).SetUseStyleTags(false, false)
	list.SetBackgroundColor(ui.palette.background)
	return list
}
