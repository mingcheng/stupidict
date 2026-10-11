package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	dictionary "github.com/mingcheng/stupidict"
	"github.com/rivo/tview"
)

func (ui *UI) render(result dictionary.LookupResult) {
	ui.current = &result
	ui.refreshPronunciation()
	ui.updateNavigation()
	ui.links.Clear()
	ui.suggestions.Clear()
	headword := result.Query
	if result.Primary != nil {
		headword = result.Primary.Word
	}
	ui.headword.SetText("[::b]" + escaped(headword) + "[::-]")
	var content strings.Builder
	if result.Primary != nil {
		entry := result.Primary
		if result.WordFormNote != "" {
			fmt.Fprintf(&content, "\n%s: %s\n", ui.tr("Word form", "词形"), escaped(result.WordFormNote))
		}
		if entry.POS != "" {
			fmt.Fprintf(&content, "\n%s\n", escaped(entry.POS))
		}
		fmt.Fprintf(&content, "\n%s\n", escaped(entry.Translation))
		if entry.Definition != "" {
			fmt.Fprintf(&content, "\n%s\n", escaped(entry.Definition))
		}
		for _, group := range []struct {
			title string
			lines []dictionary.SynonymLine
		}{
			{ui.tr("Synonyms", "近义词"), result.Synonyms}, {ui.tr("Antonyms", "反义词"), result.Antonyms},
		} {
			if len(group.lines) > 0 {
				fmt.Fprintf(&content, "\n[::b]%s[::-]\n", group.title)
			}
			for _, line := range group.lines {
				fmt.Fprintf(&content, "%s  %s\n", escaped(line.POS), escaped(strings.Join(line.Words, ", ")))
			}
		}
	} else {
		content.WriteString(ui.tr("No exact entry.", "没有精确匹配的词条。"))
	}
	seen := map[string]bool{}
	addLink := func(word string) {
		if !seen[word] {
			ui.links.AddItem(word, "", 0, nil)
			seen[word] = true
		}
	}
	for _, match := range result.ChineseMatches {
		fmt.Fprintf(&content, "\n[::b]%s[::-]\n%s\n", escaped(match.Entry.Word), escaped(match.Entry.Translation))
		addLink(match.Entry.Word)
	}
	for _, entry := range result.WordSuggestions {
		ui.suggestions.AddItem(entry.Word, "", 0, nil)
		addLink(entry.Word)
		fmt.Fprintf(&content, "\n[::b]%s[::-]\n%s\n", escaped(entry.Word), escaped(entry.Translation))
	}
	for _, lines := range [][]dictionary.SynonymLine{result.Synonyms, result.Antonyms} {
		for _, line := range lines {
			for _, word := range line.Words {
				addLink(word)
			}
		}
	}
	for _, related := range result.RelatedWords {
		addLink(related.Word)
	}
	ui.results.ScrollToBeginning().SetText(content.String())
	ui.status.SetText(clean(ui.summary(result)))
}

func (ui *UI) refreshPronunciation() {
	if ui.pronunciationCancel != nil {
		ui.pronunciationCancel()
		ui.pronunciationCancel = nil
	}
	ui.pronunciationGeneration++
	generation := ui.pronunciationGeneration
	ui.pronunciationAvailable = [2]bool{}
	ui.renderPronunciation()
	if ui.current == nil || ui.current.Primary == nil {
		return
	}
	word := ui.current.Primary.Word
	backend := ui.backend
	ctx, cancel := context.WithTimeout(ui.ctx, 10*time.Second)
	ui.pronunciationCancel = cancel
	ui.work(ctx, func(ctx context.Context) func() {
		defer cancel()
		var available [2]bool
		for index, accent := range []dictionary.Accent{dictionary.British, dictionary.American} {
			audio, err := backend.Audio(ctx, word, accent)
			available[index] = err == nil && len(audio) > 0
		}
		return func() {
			if generation != ui.pronunciationGeneration {
				return
			}
			ui.pronunciationCancel = nil
			ui.pronunciationAvailable = available
			ui.renderPronunciation()
		}
	})
}

func (ui *UI) renderPronunciation() {
	ui.pronunciation.Clear()
	phonetics := [2]string{}
	if ui.current != nil && ui.current.Primary != nil {
		phonetics = [2]string{ui.current.Primary.Phonetic, ui.current.Primary.PhoneticUS}
	}
	for index, button := range []*tview.Button{ui.british, ui.american} {
		available := ui.pronunciationAvailable[index]
		button.SetDisabled(!available)
		if !available {
			button.SetRect(0, 0, 0, 0)
			if button.HasFocus() {
				ui.app.SetFocus(ui.results)
			}
			if ui.dialogFocus == button {
				ui.dialogFocus = ui.results
			}
		}
		if phonetics[index] == "" && !available {
			continue
		}
		label := []string{"UK", "US"}[index]
		if phonetics[index] != "" {
			label += "  /" + escaped(phonetics[index]) + "/"
		}
		text := ui.textView().SetDynamicColors(true).SetScrollable(false).SetWrap(false).SetText(label)
		row := tview.NewFlex().AddItem(text, 0, 1, false)
		if available {
			row.AddItem(button, max(10, tview.TaggedStringWidth(button.GetLabel())+2), 0, false)
		}
		ui.pronunciation.AddItem(row, 1, 0, false)
	}
	ui.dictionaryPane.Clear().AddItem(ui.headword, 1, 0, false)
	if height := ui.pronunciation.GetItemCount(); height > 0 {
		ui.dictionaryPane.AddItem(ui.pronunciation, height, 0, false)
	}
	ui.dictionaryPane.AddItem(ui.results, 0, 1, false)
}
