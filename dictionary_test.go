package dictionary

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"unicode/utf16"
)

func fixture(t *testing.T, extra string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fixture with spaces.db")
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = database.Exec(`
		CREATE TABLE word(id INTEGER PRIMARY KEY, word TEXT UNIQUE, word_lower TEXT UNIQUE,
		phonetic TEXT DEFAULT '', phonetic_us TEXT DEFAULT '', pos TEXT DEFAULT '',
		translation TEXT DEFAULT '', definition TEXT DEFAULT '', freq INTEGER DEFAULT 0,
		bnc INTEGER DEFAULT 0, tag TEXT DEFAULT '');
		CREATE TABLE word_form(inflected TEXT, word_id INTEGER);
		CREATE TABLE zh_index(term TEXT, word_id INTEGER);
		CREATE TABLE syn_group(word_id INTEGER, kind TEXT, pos TEXT, words TEXT);
		CREATE TABLE audio(word TEXT PRIMARY KEY, uk BLOB, us BLOB) WITHOUT ROWID;
		INSERT INTO word(id, word, word_lower, freq) VALUES
		(1, 'cat', 'cat', 100), (2, 'catch', 'catch', 90), (3, 'catalog', 'catalog', 80),
		(4, 'Cab', 'cab', 70), (5, 'catfish', 'catfish', 0), (6, 'tiger', 'tiger', 60);
		INSERT INTO word_form VALUES ('cats', 1);
		INSERT INTO syn_group VALUES (1, 'syn', 'n.', 'tiger, absent'), (1, 'ant', 'n.', 'catch');
		INSERT INTO audio VALUES ('cat', x'010203', x'040506'), ('empty', NULL, x'');
	` + extra)
	closeErr := database.Close()
	if err != nil {
		t.Fatal(err)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	return path
}

func openFixture(t *testing.T, extra string) *Dictionary {
	t.Helper()
	dictionary, err := Open(context.Background(), fixture(t, extra))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dictionary.Close() })
	return dictionary
}

func TestEntryCount(t *testing.T) {
	dictionary := openFixture(t, "")
	count, err := dictionary.EntryCount(context.Background())
	if err != nil || count != 6 {
		t.Fatalf("entry count: %d, %v", count, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := dictionary.EntryCount(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled count: %v", err)
	}
	if err := dictionary.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := dictionary.EntryCount(context.Background()); err == nil {
		t.Fatal("closed dictionary reported a count")
	}
}

func TestLookupSemantics(t *testing.T) {
	dictionary := openFixture(t, "INSERT INTO zh_index VALUES ('\u732b', 1), ('\u732b', 1), ('\u732b', 6), ('\u732b\u79d1\u52a8\u7269', 6);")
	for _, test := range []struct {
		query   string
		tier    MatchTier
		primary string
	}{
		{"cat", Exact, "cat"}, {"  CAT  ", NormalizedExact, "cat"}, {"Cats", WordForm, "cat"},
		{"ca", Prefix, ""}, {"cath", Fuzzy, ""}, {"\u732b", Chinese, "tiger"},
		{"\u732b\u79d1", Prefix, ""}, {"zzzzzzzzzzzz", NotFound, ""}, {"  ", NotFound, ""},
	} {
		t.Run(test.query, func(t *testing.T) {
			result, err := dictionary.Lookup(context.Background(), test.query)
			if err != nil {
				t.Fatal(err)
			}
			primary := ""
			if result.Primary != nil {
				primary = result.Primary.Word
			}
			if result.Tier != test.tier || primary != test.primary {
				t.Fatalf("got %s/%q, want %s/%q", result.Tier, primary, test.tier, test.primary)
			}
			if test.tier == WordForm && (result.WordFormNote != "cat" || len(result.Synonyms) != 1) {
				t.Fatalf("word form: %+v", result)
			}
			if test.tier == Chinese && len(result.ChineseMatches) != 2 {
				t.Fatalf("duplicate Chinese matches: %+v", result.ChineseMatches)
			}
			if (result.Primary == nil || result.IsChineseQuery) && len(result.Synonyms) != 0 {
				t.Fatal("unexpected thesaurus")
			}
		})
	}
	result, err := dictionary.Lookup(context.Background(), "ca")
	if err != nil {
		t.Fatal(err)
	}
	if result.WordSuggestions[0].Word != "Cab" {
		t.Fatalf("prefix ranking: %+v", result.WordSuggestions)
	}
	if _, err := dictionary.database.Exec("DELETE FROM word"); err == nil {
		t.Fatal("database is not read-only")
	}
}

func TestSuggestions(t *testing.T) {
	dictionary := openFixture(t, `INSERT INTO word(word, word_lower, freq, bnc) VALUES
		('aaba', 'aaba', 0, 1000), ('aabb', 'aabb', 5000, 0);`)
	for _, test := range []struct {
		query    string
		expected []string
	}{
		{"ca", []string{"Cab", "catalog", "catch", "cat"}},
		{"  CA  ", []string{"Cab", "catalog", "catch", "cat"}},
		{"cat", []string{"cat", "catalog", "catch", "catfish"}},
		{"aa", []string{"aaba", "aabb"}}, {"aab", []string{"aaba", "aabb"}},
		{"", []string{}}, {"\u732b", []string{}},
	} {
		actual, err := dictionary.Suggest(context.Background(), test.query)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(actual, test.expected) {
			t.Fatalf("%q: got %v, want %v", test.query, actual, test.expected)
		}
	}
}

func TestThesaurusAndLegacySchema(t *testing.T) {
	dictionary := openFixture(t, "UPDATE word SET translation = 'adj. \u7a00\u6709\u7684, \u7f55\u89c1\u7684\uff1b\u73cd\u8d35\u7684' WHERE word = 'tiger';")
	result, err := dictionary.Lookup(context.Background(), "cat")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Synonyms) != 1 || len(result.Antonyms) != 1 || len(result.RelatedWords) != 2 {
		t.Fatalf("thesaurus: %+v", result)
	}
	if result.RelatedWords[0] != (RelatedWord{Word: "tiger", Gloss: "\u7a00\u6709\u7684\uff0c\u7f55\u89c1\u7684"}) {
		t.Fatalf("related words: %+v", result.RelatedWords)
	}
	legacy := openFixture(t, "DROP TABLE syn_group; ALTER TABLE word DROP COLUMN phonetic_us;")
	result, err = legacy.Lookup(context.Background(), "cat")
	if err != nil || result.Primary == nil || result.Primary.PhoneticUS != "" || len(result.Synonyms) != 0 {
		t.Fatalf("legacy: %+v, %v", result, err)
	}
}

func TestUnicodeAndFuzzy(t *testing.T) {
	for _, query := range []string{"\u3007", "\u732b", "\U00020000", "\U0002ec00", "\U000323b0", "abc\u3007"} {
		if !IsChineseQuery(query) {
			t.Fatalf("not Chinese: %q", query)
		}
	}
	for _, query := range []string{"cat", "\u3001", "\u3002", "\U0001f600", "\uff23\uff21\uff34"} {
		if IsChineseQuery(query) {
			t.Fatalf("unexpected Chinese: %q", query)
		}
	}
	for _, test := range []struct {
		left, right      string
		cutoff, distance int
	}{
		{"cta", "cat", 1, 1}, {"abcd", "badc", 1, -1}, {"CA", "ABC", 3, 3},
		{"kitten", "sitting", 3, 3}, {"same", "same", 1, 0}, {"", "ab", 1, -1},
	} {
		actual := boundedDistance(utf16.Encode([]rune(test.left)), utf16.Encode([]rune(test.right)), test.cutoff)
		if actual != test.distance {
			t.Fatalf("distance %q/%q = %d, want %d", test.left, test.right, actual, test.distance)
		}
	}
	candidates := []commonWord{{"cut", utf16.Encode([]rune("cut")), 5000}, {"cot", utf16.Encode([]rune("cot")), 1000}}
	actual, err := fuzzyWords(context.Background(), "cat", candidates, 10)
	if err != nil || !reflect.DeepEqual(actual, []string{"cot", "cut"}) {
		t.Fatalf("fuzzy ranking: %v %v", actual, err)
	}
}

func TestCancellationAndConcurrentReads(t *testing.T) {
	dictionary := openFixture(t, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := dictionary.Warmup(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("warmup cancellation: %v", err)
	}
	if _, err := dictionary.Lookup(ctx, "cat"); !errors.Is(err, context.Canceled) {
		t.Fatalf("lookup cancellation: %v", err)
	}
	if _, err := dictionary.Suggest(ctx, "ca"); !errors.Is(err, context.Canceled) {
		t.Fatalf("suggest cancellation: %v", err)
	}
	var workers sync.WaitGroup
	for index := 0; index < 8; index++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if _, err := dictionary.Suggest(context.Background(), "ca"); err != nil {
				t.Error(err)
			}
			if _, err := dictionary.Lookup(context.Background(), "cath"); err != nil {
				t.Error(err)
			}
		}()
	}
	workers.Wait()
}

func TestMissingDatabaseDoesNotCreateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.db")
	if database, err := Open(context.Background(), path); err == nil {
		database.Close()
		t.Fatal("opened missing database")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing path changed: %v", err)
	}
}

func TestRealDictionary(t *testing.T) {
	path := os.Getenv("STUPIDDICT_DICTIONARY")
	if path == "" {
		t.Skip("set STUPIDDICT_DICTIONARY to test the downloaded dictionary")
	}
	ctx := context.Background()
	dictionary, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dictionary.Close() })
	var count int
	var check string
	if err := dictionary.database.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&check); err != nil || check != "ok" {
		t.Fatalf("quick_check: %q %v", check, err)
	}
	if err := dictionary.database.QueryRowContext(ctx, "SELECT COUNT(*) FROM word").Scan(&count); err != nil {
		t.Fatal(err)
	}
	t.Logf("dictionary: %d headwords, quick_check=%s", count, check)
	for _, query := range []string{"cat", "  CAT  ", "cats", "\u732b", "cath", "dictionray", "good", "zzzzzzzzzzzzzzzzzzzz"} {
		result, err := dictionary.Lookup(ctx, query)
		if err != nil {
			t.Fatal(err)
		}
		if query == "cat" && (result.Tier != Exact || result.Primary == nil || result.Primary.Word != "cat" || result.Primary.Translation == "") {
			t.Fatalf("cat: %+v", result)
		}
		if query == "  CAT  " && (result.Tier != NormalizedExact || result.Primary == nil || result.Primary.Word != "cat") {
			t.Fatalf("CAT: %+v", result)
		}
		if query == "\u732b" && (result.Tier != Chinese || len(result.ChineseMatches) == 0) {
			t.Fatalf("Chinese: %+v", result)
		}
		if query == "dictionray" && (result.Tier != Fuzzy || result.Primary != nil || len(result.WordSuggestions) == 0 || result.WordSuggestions[0].Word != "dictionary") {
			t.Fatalf("fuzzy: %+v", result)
		}
		if query == "good" && (len(result.Synonyms) == 0 || len(result.RelatedWords) == 0) {
			t.Fatalf("thesaurus: %+v", result)
		}
		if query == "zzzzzzzzzzzzzzzzzzzz" && result.Tier != NotFound {
			t.Fatalf("miss: %+v", result)
		}
		headword := ""
		if result.Primary != nil {
			headword = result.Primary.Word
		}
		t.Logf("%q: tier=%s primary=%q suggestions=%d chinese=%d synonyms=%d", query, result.Tier, headword, len(result.WordSuggestions), len(result.ChineseMatches), len(result.Synonyms))
	}
	for _, prefix := range []string{"ca", "cat", "zebra"} {
		words, err := dictionary.Suggest(ctx, prefix)
		if err != nil {
			t.Fatal(err)
		}
		if len(words) == 0 || len(words) > 8 {
			t.Fatalf("suggest %q: %v", prefix, words)
		}
		if prefix == "cat" && words[0] != "cat" {
			t.Fatalf("cat not first: %v", words)
		}
		t.Logf("suggest %q: %v", prefix, words)
	}
}

func BenchmarkRealDictionary(b *testing.B) {
	path := os.Getenv("STUPIDDICT_DICTIONARY")
	if path == "" {
		b.Skip("set STUPIDDICT_DICTIONARY")
	}
	ctx := context.Background()
	dictionary, err := Open(ctx, path)
	if err != nil {
		b.Fatal(err)
	}
	defer dictionary.Close()
	if err := dictionary.Warmup(ctx); err != nil {
		b.Fatal(err)
	}
	for _, query := range []string{"cat", "\u732b", "dictionray"} {
		b.Run("lookup/"+query, func(b *testing.B) {
			b.ReportAllocs()
			for index := 0; index < b.N; index++ {
				if _, err := dictionary.Lookup(ctx, query); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
	for _, query := range []string{"ca", "cat"} {
		b.Run("suggest/"+query, func(b *testing.B) {
			b.ReportAllocs()
			for index := 0; index < b.N; index++ {
				if _, err := dictionary.Suggest(ctx, query); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
