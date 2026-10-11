package dictionary

import (
	"context"
	"regexp"
	"sort"
	"strings"
)

const commonalitySQL = "CASE WHEN w.freq > 0 THEN w.freq WHEN w.bnc > 0 THEN w.bnc ELSE 999999 END"

func entryDest(entry *Entry) []any {
	return []any{&entry.Word, &entry.Phonetic, &entry.PhoneticUS, &entry.POS, &entry.Translation, &entry.Definition, &entry.Frequency, &entry.BNC, &entry.Tag}
}

func (dictionary *Dictionary) entries(ctx context.Context, clause string, arguments ...any) ([]Entry, error) {
	rows, err := dictionary.database.QueryContext(ctx, "SELECT "+dictionary.columns+" "+clause, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := []Entry{}
	for rows.Next() {
		var entry Entry
		if err := rows.Scan(entryDest(&entry)...); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

func (dictionary *Dictionary) prefix(ctx context.Context, lower string, limit int) ([]Entry, error) {
	return dictionary.entries(ctx, "FROM word w WHERE w.word_lower >= ? AND w.word_lower < ? ORDER BY "+commonalitySQL+", w.word_lower LIMIT ?", lower, lower+"\U0010ffff", limit)
}

func (dictionary *Dictionary) chineseMatches(ctx context.Context, term string, prefix bool) ([]ChineseMatch, error) {
	condition := "z.term = ?"
	arguments := []any{term}
	if prefix {
		condition = "z.term >= ? AND z.term < ?"
		arguments = append(arguments, term+"\U0010ffff")
	}
	rows, err := dictionary.database.QueryContext(ctx, "SELECT DISTINCT z.term, "+dictionary.columns+" FROM zh_index z JOIN word w ON w.id = z.word_id WHERE "+condition+" ORDER BY "+commonalitySQL+", w.word LIMIT 12", arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	matches := []ChineseMatch{}
	for rows.Next() {
		var match ChineseMatch
		if err := rows.Scan(append([]any{&match.Term}, entryDest(&match.Entry)...)...); err != nil {
			return nil, err
		}
		matches = append(matches, match)
	}
	return matches, rows.Err()
}

func (dictionary *Dictionary) thesaurus(ctx context.Context, lower string, result *LookupResult) error {
	if !dictionary.hasThesaurus {
		return nil
	}
	rows, err := dictionary.database.QueryContext(ctx, "SELECT g.kind, g.pos, g.words FROM syn_group g JOIN word w ON w.id = g.word_id WHERE w.word_lower = ? ORDER BY g.rowid", lower)
	if err != nil {
		return err
	}
	pool := map[string]bool{}
	for rows.Next() {
		var kind, pos, words string
		if err := rows.Scan(&kind, &pos, &words); err != nil {
			rows.Close()
			return err
		}
		line := SynonymLine{POS: pos, Words: []string{}}
		for _, word := range strings.Split(words, ", ") {
			word = strings.TrimSpace(word)
			if word == "" {
				continue
			}
			line.Words = append(line.Words, word)
			pool[strings.ToLower(word)] = true
		}
		if kind == "syn" {
			result.Synonyms = append(result.Synonyms, line)
		} else {
			result.Antonyms = append(result.Antonyms, line)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if len(pool) == 0 {
		return nil
	}
	arguments := make([]any, 0, len(pool))
	for word := range pool {
		arguments = append(arguments, word)
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(arguments)), ",")
	entries, err := dictionary.entries(ctx, "FROM word w WHERE w.word_lower IN ("+placeholders+")", arguments...)
	if err != nil {
		return err
	}
	sort.Slice(entries, func(left, right int) bool {
		leftRank, rightRank := rank(entries[left].Frequency, entries[left].BNC), rank(entries[right].Frequency, entries[right].BNC)
		if leftRank != rightRank {
			return leftRank < rightRank
		}
		return ordinalLess(entries[left].Word, entries[right].Word)
	})
	if len(entries) > 10 {
		entries = entries[:10]
	}
	for _, entry := range entries {
		result.RelatedWords = append(result.RelatedWords, RelatedWord{Word: entry.Word, Gloss: firstGloss(entry.Translation)})
	}
	return nil
}

var posPrefix = regexp.MustCompile(`^[a-zA-Z]+\.`)

func firstGloss(translation string) string {
	line := ""
	for _, candidate := range strings.Split(translation, "\n") {
		if line = strings.TrimSpace(candidate); line != "" {
			break
		}
	}
	if prefix := posPrefix.FindString(line); prefix != "" {
		line = strings.TrimLeftFunc(strings.TrimPrefix(line, prefix), isSpace)
	}
	line = strings.SplitN(line, "\uff1b", 2)[0]
	line = strings.TrimRight(line, "\u3002\uff0c\u2026; ")
	chunks := []string{}
	for _, chunk := range strings.FieldsFunc(line, func(character rune) bool { return character == ',' || character == '\uff0c' }) {
		if chunk = strings.TrimSpace(chunk); chunk != "" {
			chunks = append(chunks, chunk)
		}
		if len(chunks) == 2 {
			break
		}
	}
	return strings.Join(chunks, "\uff0c")
}
