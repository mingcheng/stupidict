package profile

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestConfigRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings", "config.json")
	config, err := Load(path)
	if err != nil || config.Theme != "system" {
		t.Fatalf("defaults: %+v %v", config, err)
	}
	if config.Dictionary != filepath.Join(filepath.Dir(path), "dictionary.db") || config.Audio != filepath.Join(filepath.Dir(path), "audio-pack.db") {
		t.Fatalf("default assets must live with the profile: %+v", config)
	}
	config.Dictionary, config.Audio, config.Glyphs = "dict.db", "", "emoji"
	if err := Save(path, config); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil || loaded.Dictionary != filepath.Join(filepath.Dir(path), "dict.db") || loaded.Audio != "" || loaded.Glyphs != "emoji" {
		t.Fatalf("round trip: %+v %v", loaded, err)
	}
	config.Theme = "light"
	if err := Save(path, config); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	config.Theme = "invalid"
	if err := Save(path, config); err == nil {
		t.Fatal("invalid config saved")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("failed save changed configuration")
	}
	for _, invalid := range []string{"{", "null", `{"theme":"invalid"}`, `{"language":"xx"}`, `{"glyphs":"xx"}`, `{"dictionary":[]}`, `{"dictionary":"history.db"}`, `{"audio":"config.json"}`, `{"audio":"dictionary.db"}`} {
		if err := os.WriteFile(path, []byte(invalid), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Fatalf("accepted %s", invalid)
		}
	}
}

func TestDefaultProfileUsesHomeDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "other"))
	path, err := DefaultPath()
	if err != nil || path != filepath.Join(home, ".config", "stupidict", "config.json") {
		t.Fatalf("default profile: %s %v", path, err)
	}
}

func TestHistoryPersistenceAndEditing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "user data", "history.db")
	history, err := OpenHistory(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { history.Close() }()
	for _, query := range []string{"cat", "dog", "cat"} {
		if err := history.Visit(query); err != nil {
			t.Fatal(err)
		}
	}
	records, err := history.List()
	if err != nil || len(records) != 2 || records[0].Query != "cat" {
		t.Fatalf("history: %+v %v", records, err)
	}
	if err := history.Edit(records[0].ID, "dog"); err == nil {
		t.Fatal("duplicate edit should fail")
	}
	if err := history.Edit(records[0].ID, "kitten"); err != nil {
		t.Fatal(err)
	}
	if err := history.Close(); err != nil {
		t.Fatal(err)
	}
	history, err = OpenHistory(path)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := history.List()
	if err != nil || len(saved) != 2 || saved[0].Query != "kitten" {
		t.Fatalf("persist: %+v %v", saved, err)
	}
	if err := history.Delete(records[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := history.Clear(); err != nil {
		t.Fatal(err)
	}
	remaining, err := history.List()
	if err != nil || len(remaining) != 0 {
		t.Fatalf("clear: %+v %v", remaining, err)
	}
	if err := history.Edit(-1, "missing"); err == nil {
		t.Fatal("missing edit accepted")
	}
	if err := history.Visit(" "); err == nil {
		t.Fatal("blank query accepted")
	}
}

func TestHistoryBoundAndIndependentClients(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	first, err := OpenHistory(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := OpenHistory(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if err := first.Visit("first"); err != nil {
		t.Fatal(err)
	}
	if err := second.Visit("second"); err != nil {
		t.Fatal(err)
	}
	records, err := first.List()
	if err != nil || len(records) != 2 {
		t.Fatalf("shared records: %+v %v", records, err)
	}
	for index := 0; index < 35; index++ {
		if err := first.Visit(fmt.Sprint(index)); err != nil {
			t.Fatal(err)
		}
	}
	records, err = first.List()
	if err != nil || len(records) != 30 || records[0].Query != "34" {
		t.Fatalf("history bound: %d %v", len(records), err)
	}
}
