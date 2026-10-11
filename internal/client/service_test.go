package client

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	dictionary "github.com/mingcheng/stupidict"
	"github.com/mingcheng/stupidict/internal/assets"
	"github.com/mingcheng/stupidict/internal/profile"
)

type fakeEngine struct{ closed bool }

type countedEngine struct{ fakeEngine }

func (engine *countedEngine) EntryCount(ctx context.Context) (int64, error) {
	return 42, ctx.Err()
}

func TestDataInfo(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	service, err := Open(context.Background(), path, profile.Defaults(), func(context.Context, string) (Engine, error) {
		return &countedEngine{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	config := service.Config()
	modified := time.Date(2025, 4, 3, 12, 0, 0, 0, time.UTC)
	if err := os.WriteFile(config.Dictionary, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(config.Dictionary, modified, modified); err != nil {
		t.Fatal(err)
	}
	info := service.DataInfo(context.Background())
	if info.Entries != 42 || info.CountError != nil || info.Dictionary.Err != nil || info.Dictionary.Size != 7 || !info.Dictionary.Modified.Equal(modified) {
		t.Fatalf("data info: %+v", info)
	}
	if !errors.Is(info.Audio.Err, os.ErrNotExist) {
		t.Fatalf("missing audio: %+v", info.Audio)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if info := service.DataInfo(ctx); !errors.Is(info.CountError, context.Canceled) {
		t.Fatalf("canceled count: %+v", info)
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	if info := service.DataInfo(context.Background()); info.CountError == nil {
		t.Fatal("closed service reported valid count")
	}
}

func (engine *fakeEngine) Close() error { engine.closed = true; return nil }
func (engine *fakeEngine) Lookup(ctx context.Context, query string) (dictionary.LookupResult, error) {
	return dictionary.LookupResult{Query: query, Primary: &dictionary.Entry{Word: query}}, ctx.Err()
}

func TestInitializeInstallsAndReloadsDictionaryBeforeAudio(t *testing.T) {
	fixture := filepath.Join(t.TempDir(), "fixture.db")
	database, err := sql.Open("sqlite", fixture)
	if err != nil {
		t.Fatal(err)
	}
	_, err = database.Exec(`CREATE TABLE word(word TEXT PRIMARY KEY,phonetic TEXT,pos TEXT,translation TEXT,definition TEXT,freq INTEGER,bnc INTEGER,tag TEXT); INSERT INTO word VALUES('cat','','n','猫','cat',1,1,'')`)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	var buffer bytes.Buffer
	archive := zip.NewWriter(&buffer)
	entry, err := archive.Create("dictionary.db")
	if err != nil {
		t.Fatal(err)
	}
	entry.Write(data)
	archive.Close()
	service, err := Open(context.Background(), filepath.Join(t.TempDir(), "config.json"), profile.Defaults(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		if strings.HasSuffix(req.URL.Path, ".sha256") {
			writer.WriteHeader(404)
			return
		}
		if strings.HasSuffix(req.URL.Path, assets.Dictionary) {
			writer.Write(buffer.Bytes())
			return
		}
		if !service.Ready() {
			t.Error("audio download began before dictionary was available")
		}
		cancel()
		<-req.Context().Done()
	}))
	defer server.Close()
	err = service.Initialize(ctx, &assets.Downloader{Sources: []string{server.URL + "/"}}, false, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("audio cancellation: %v", err)
	}
	result, err := service.Lookup(context.Background(), "cat")
	if err != nil || result.Primary == nil || result.Primary.Translation != "猫" {
		t.Fatalf("new dictionary unavailable: %+v %v", result, err)
	}
	installed, _ := os.ReadFile(service.Config().Dictionary)
	if !bytes.Equal(data, installed) {
		t.Fatal("installed dictionary differs")
	}
	info := service.DataInfo(context.Background())
	if info.CountError != nil || info.Entries != 1 || info.Dictionary.Err != nil || info.Dictionary.Size != int64(len(data)) {
		t.Fatalf("installed dictionary statistics: %+v", info)
	}
	if _, err := os.Stat(service.Config().Audio); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed audio looks installed")
	}
}

func TestInitializationDoesNotReplaceUnknownExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	config, err := profile.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.Dictionary, []byte("unrecognized data"), 0600); err != nil {
		t.Fatal(err)
	}
	service, err := Open(context.Background(), path, config, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	if err := service.Initialize(context.Background(), &assets.Downloader{}, false, nil); err == nil {
		t.Fatal("unknown existing file accepted")
	}
	data, _ := os.ReadFile(config.Dictionary)
	if string(data) != "unrecognized data" {
		t.Fatal("existing file overwritten")
	}
}
func (engine *fakeEngine) Suggest(ctx context.Context, query string) ([]string, error) {
	return []string{query}, ctx.Err()
}

func TestServiceAcceptAndConfigure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	config := profile.Defaults()
	old := &fakeEngine{}
	factory := func(_ context.Context, path string) (Engine, error) {
		if filepath.Base(path) == "bad.db" {
			return nil, errors.New("invalid dictionary")
		}
		return old, nil
	}
	service, err := Open(context.Background(), path, config, factory)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	result, err := service.Lookup(context.Background(), "cat")
	if err != nil {
		t.Fatal(err)
	}
	records, _ := service.History()
	if len(records) != 0 {
		t.Fatal("lookup wrote history before acceptance")
	}
	if err := service.Accept(result); err != nil {
		t.Fatal(err)
	}
	if err := service.Accept(dictionary.LookupResult{Query: "missing"}); err != nil {
		t.Fatal(err)
	}
	records, _ = service.History()
	if len(records) != 1 {
		t.Fatalf("history: %+v", records)
	}
	config.Theme = "light"
	if err := service.Configure(context.Background(), config); err != nil {
		t.Fatal(err)
	}
	loaded, err := profile.Load(path)
	if err != nil || loaded.Theme != "light" {
		t.Fatalf("config: %+v %v", loaded, err)
	}
	config.Dictionary = "bad.db"
	if err := service.Configure(context.Background(), config); err == nil {
		t.Fatal("bad replacement accepted")
	}
	if old.closed || service.Config().Dictionary != loaded.Dictionary {
		t.Fatal("failed config changed active engine")
	}
}

func TestConfigureSaveFailureClosesOnlyReplacement(t *testing.T) {
	config := profile.Defaults()
	old, replacement := &fakeEngine{}, &fakeEngine{}
	calls := 0
	service, err := Open(context.Background(), filepath.Join(t.TempDir(), "config.json"), config, func(context.Context, string) (Engine, error) {
		calls++
		if calls == 1 {
			return old, nil
		}
		return replacement, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	service.path = t.TempDir() // Replacing a directory with a file must fail.
	config.Dictionary = "replacement.db"
	if err := service.Configure(context.Background(), config); err == nil {
		t.Fatal("save should fail")
	}
	if old.closed || !replacement.closed {
		t.Fatal("incorrect engine lifetime after save failure")
	}
}

func TestMissingAssetsCanBeReconfigured(t *testing.T) {
	config := profile.Defaults()
	calls := 0
	service, err := Open(context.Background(), filepath.Join(t.TempDir(), "config.json"), config, func(context.Context, string) (Engine, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("missing assets")
		}
		return &fakeEngine{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	if _, err := service.Lookup(context.Background(), "cat"); err == nil {
		t.Fatal("missing engine")
	}
	if err := service.Configure(context.Background(), config); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Lookup(context.Background(), "cat"); err != nil {
		t.Fatal(err)
	}
}

func TestNavigation(t *testing.T) {
	var navigation Navigation
	if _, ok := navigation.Move(-1); ok {
		t.Fatal("empty back")
	}
	for _, query := range []string{"cat", "cat", "dog"} {
		navigation.Push(dictionary.LookupResult{Query: query})
	}
	result, ok := navigation.Move(-1)
	if !ok || result.Query != "cat" || navigation.CanBack() || !navigation.CanForward() {
		t.Fatal("back/duplicate semantics")
	}
	navigation.Push(dictionary.LookupResult{Query: "bird"})
	if navigation.CanForward() {
		t.Fatal("branch retained forward page")
	}
}
