package dictionary

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestAudioSemantics(t *testing.T) {
	ctx := context.Background()
	store, err := OpenAudio(ctx, fixture(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	for _, test := range []struct {
		accent   Accent
		expected []byte
	}{
		{British, []byte{1, 2, 3}}, {American, []byte{4, 5, 6}},
	} {
		audio, err := store.GetAudio(ctx, "CAT", test.accent)
		if err != nil || !bytes.Equal(audio, test.expected) {
			t.Fatalf("audio: %v %v", audio, err)
		}
		if _, err := store.GetAudio(ctx, "empty", test.accent); !errors.Is(err, ErrAudioNotFound) {
			t.Fatalf("empty: %v", err)
		}
	}
	if _, err := store.GetAudio(ctx, "missing", British); !errors.Is(err, ErrAudioNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if _, err := store.GetAudio(ctx, "cat", "invalid"); err == nil {
		t.Fatal("accepted invalid accent")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := store.GetAudio(canceled, "cat", British); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	if _, err := store.database.Exec("DELETE FROM audio"); err == nil {
		t.Fatal("audio is writable")
	}
	path := filepath.Join(t.TempDir(), "broken.db")
	if err := os.WriteFile(path, []byte("not sqlite"), 0600); err != nil {
		t.Fatal(err)
	}
	broken, err := OpenAudio(ctx, path)
	if err == nil {
		defer broken.Close()
		_, err = broken.GetAudio(ctx, "cat", British)
	}
	if err == nil || errors.Is(err, ErrAudioNotFound) {
		t.Fatalf("corruption must differ from a miss: %v", err)
	}
}

func TestRealAudio(t *testing.T) {
	path := os.Getenv("STUPIDDICT_AUDIO")
	if path == "" {
		t.Skip("set STUPIDDICT_AUDIO to test the downloaded pack")
	}
	ctx := context.Background()
	store, err := OpenAudio(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	var count, britishCount, americanCount int
	var check string
	if err := store.database.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&check); err != nil || check != "ok" {
		t.Fatalf("quick_check: %q %v", check, err)
	}
	if err := store.database.QueryRowContext(ctx, "SELECT COUNT(*), COUNT(CASE WHEN length(uk) > 0 THEN 1 END), COUNT(CASE WHEN length(us) > 0 THEN 1 END) FROM audio").Scan(&count, &britishCount, &americanCount); err != nil {
		t.Fatal(err)
	}
	t.Logf("audio: %d words, uk=%d, us=%d, quick_check=%s", count, britishCount, americanCount, check)
	for _, accent := range []Accent{British, American} {
		audio, err := store.GetAudio(ctx, "CAT", accent)
		if err != nil {
			t.Fatal(err)
		}
		var expected []byte
		if err := store.database.QueryRowContext(ctx, "SELECT "+string(accent)+" FROM audio WHERE word = 'cat'").Scan(&expected); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(audio, expected) || len(audio) == 0 {
			t.Fatal("MP3 bytes differ from stored blob")
		}
		t.Logf("cat %s: %d bytes, SHA-256 %x", accent, len(audio), sha256.Sum256(audio))
	}
	if _, err := store.GetAudio(ctx, "__missing_word__", British); !errors.Is(err, ErrAudioNotFound) {
		t.Fatalf("missing word: %v", err)
	}
}

func BenchmarkRealAudio(b *testing.B) {
	path := os.Getenv("STUPIDDICT_AUDIO")
	if path == "" {
		b.Skip("set STUPIDDICT_AUDIO")
	}
	ctx := context.Background()
	store, err := OpenAudio(ctx, path)
	if err != nil {
		b.Fatal(err)
	}
	defer store.Close()
	if _, err := store.GetAudio(ctx, "cat", American); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if _, err := store.GetAudio(ctx, "cat", American); err != nil {
			b.Fatal(err)
		}
	}
}
