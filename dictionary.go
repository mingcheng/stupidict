package dictionary

import (
	"context"
	"database/sql"
	"strings"
	"sync"
)

type MatchTier string

const (
	Exact           MatchTier = "Exact"
	NormalizedExact MatchTier = "NormalizedExact"
	WordForm        MatchTier = "WordForm"
	Chinese         MatchTier = "Chinese"
	Prefix          MatchTier = "Prefix"
	Fuzzy           MatchTier = "Fuzzy"
	NotFound        MatchTier = "NotFound"
)

type Entry struct {
	Word        string `json:"word"`
	Phonetic    string `json:"phonetic"`
	PhoneticUS  string `json:"phonetic_us"`
	POS         string `json:"pos"`
	Translation string `json:"translation"`
	Definition  string `json:"definition"`
	Frequency   int    `json:"freq"`
	BNC         int    `json:"bnc"`
	Tag         string `json:"tag"`
}

type ChineseMatch struct {
	Term  string `json:"term"`
	Entry Entry  `json:"entry"`
}

type SynonymLine struct {
	POS   string   `json:"pos"`
	Words []string `json:"words"`
}

type RelatedWord struct {
	Word  string `json:"word"`
	Gloss string `json:"gloss"`
}

type LookupResult struct {
	Query           string         `json:"query"`
	IsChineseQuery  bool           `json:"is_chinese_query"`
	Tier            MatchTier      `json:"tier"`
	Primary         *Entry         `json:"primary"`
	WordFormNote    string         `json:"word_form_note,omitempty"`
	WordSuggestions []Entry        `json:"word_suggestions"`
	ChineseMatches  []ChineseMatch `json:"chinese_matches"`
	Synonyms        []SynonymLine  `json:"synonyms"`
	Antonyms        []SynonymLine  `json:"antonyms"`
	RelatedWords    []RelatedWord  `json:"related_words"`
}

type Dictionary struct {
	database     *sql.DB
	columns      string
	hasThesaurus bool
	commonGate   sync.Mutex
	common       []commonWord
}

func Open(ctx context.Context, path string) (*Dictionary, error) {
	database, err := openReadOnly(ctx, path)
	if err != nil {
		return nil, err
	}
	dictionary := &Dictionary{database: database}
	var hasUS, hasThesaurus int
	err = database.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM pragma_table_info('word') WHERE name = 'phonetic_us'), EXISTS(SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'syn_group')").Scan(&hasUS, &hasThesaurus)
	if err != nil {
		database.Close()
		return nil, err
	}
	phoneticUS := "''"
	if hasUS != 0 {
		phoneticUS = "COALESCE(w.phonetic_us, '')"
	}
	dictionary.columns = "w.word, COALESCE(w.phonetic, ''), " + phoneticUS + ", COALESCE(w.pos, ''), COALESCE(w.translation, ''), COALESCE(w.definition, ''), COALESCE(w.freq, 0), COALESCE(w.bnc, 0), COALESCE(w.tag, '')"
	dictionary.hasThesaurus = hasThesaurus != 0
	rows, err := database.QueryContext(ctx, "SELECT "+dictionary.columns+" FROM word w LIMIT 0")
	if err != nil {
		database.Close()
		return nil, err
	}
	rows.Close()
	return dictionary, nil
}

func (dictionary *Dictionary) Close() error { return dictionary.database.Close() }

func (dictionary *Dictionary) EntryCount(ctx context.Context) (int64, error) {
	var count int64
	err := dictionary.database.QueryRowContext(ctx, "SELECT COUNT(*) FROM word").Scan(&count)
	return count, err
}

func (dictionary *Dictionary) Warmup(ctx context.Context) error {
	_, err := dictionary.commonWords(ctx)
	return err
}

func (dictionary *Dictionary) Lookup(ctx context.Context, query string) (LookupResult, error) {
	query = strings.TrimSpace(query)
	result := LookupResult{Query: query, IsChineseQuery: IsChineseQuery(query), Tier: NotFound,
		WordSuggestions: []Entry{}, ChineseMatches: []ChineseMatch{}, Synonyms: []SynonymLine{}, Antonyms: []SynonymLine{}, RelatedWords: []RelatedWord{}}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if query == "" {
		return result, nil
	}
	if result.IsChineseQuery {
		matches, err := dictionary.chineseMatches(ctx, query, false)
		if err != nil {
			return result, err
		}
		if len(matches) > 0 {
			result.Tier, result.Primary, result.ChineseMatches = Chinese, &matches[0].Entry, matches
			return result, nil
		}
		matches, err = dictionary.chineseMatches(ctx, query, true)
		if err != nil {
			return result, err
		}
		if len(matches) > 0 {
			result.Tier, result.ChineseMatches = Prefix, matches
		}
		return result, nil
	}
	lower := strings.ToLower(query)
	for _, attempt := range []struct {
		tier     MatchTier
		clause   string
		argument string
	}{
		{Exact, "FROM word w WHERE w.word = ? LIMIT 1", query},
		{NormalizedExact, "FROM word w WHERE w.word_lower = ? LIMIT 1", lower},
		{WordForm, "FROM word_form f JOIN word w ON w.id = f.word_id WHERE f.inflected = ? ORDER BY " + commonalitySQL + ", w.word LIMIT 1", lower},
	} {
		entries, err := dictionary.entries(ctx, attempt.clause, attempt.argument)
		if err != nil {
			return result, err
		}
		if len(entries) == 0 {
			continue
		}
		result.Primary, result.Tier = &entries[0], attempt.tier
		if attempt.tier == WordForm {
			result.WordFormNote = entries[0].Word
		}
		return result, dictionary.thesaurus(ctx, strings.ToLower(entries[0].Word), &result)
	}
	prefix, err := dictionary.prefix(ctx, lower, 10)
	if err != nil {
		return result, err
	}
	if len(prefix) > 0 {
		result.Tier = Prefix
	}
	result.WordSuggestions = prefix
	seen := make(map[string]bool)
	for _, entry := range prefix {
		seen[entry.Word] = true
	}
	common, err := dictionary.commonWords(ctx)
	if err != nil {
		return result, err
	}
	similar, err := fuzzyWords(ctx, lower, common, 10)
	if err != nil {
		return result, err
	}
	for _, word := range similar {
		entries, err := dictionary.entries(ctx, "FROM word w WHERE w.word_lower = ? LIMIT 1", word)
		if err != nil {
			return result, err
		}
		if len(entries) > 0 && !seen[entries[0].Word] {
			seen[entries[0].Word] = true
			result.WordSuggestions = append(result.WordSuggestions, entries[0])
		}
	}
	if len(result.WordSuggestions) > 10 {
		result.WordSuggestions = result.WordSuggestions[:10]
	}
	if result.Tier == NotFound && len(result.WordSuggestions) > 0 {
		result.Tier = Fuzzy
	}
	return result, nil
}

func (dictionary *Dictionary) Suggest(ctx context.Context, query string) ([]string, error) {
	words := []string{}
	query = strings.TrimSpace(query)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if query == "" || IsChineseQuery(query) {
		return words, nil
	}
	lower := strings.ToLower(query)
	if utf16Length(lower) <= 2 {
		common, err := dictionary.commonWords(ctx)
		if err != nil {
			return nil, err
		}
		for _, candidate := range common {
			if !strings.HasPrefix(candidate.word, lower) {
				continue
			}
			entries, err := dictionary.entries(ctx, "FROM word w WHERE w.word_lower = ? LIMIT 1", candidate.word)
			if err != nil {
				return nil, err
			}
			if len(entries) > 0 {
				words = append(words, entries[0].Word)
			}
			if len(words) == 8 {
				break
			}
		}
	} else {
		entries, err := dictionary.prefix(ctx, lower, 8)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			words = append(words, entry.Word)
		}
	}
	exact, err := dictionary.entries(ctx, "FROM word w WHERE w.word_lower = ? LIMIT 1", lower)
	if err != nil {
		return nil, err
	}
	if len(exact) > 0 {
		leading := []string{exact[0].Word}
		for _, word := range words {
			if word != exact[0].Word {
				leading = append(leading, word)
			}
		}
		words = leading
		if len(words) > 8 {
			words = words[:8]
		}
	}
	return words, nil
}
