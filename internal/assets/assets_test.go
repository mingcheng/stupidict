package assets

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

func audioFixture(t *testing.T) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "audio.db")
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = database.Exec(`CREATE TABLE audio(word TEXT PRIMARY KEY,uk BLOB,us BLOB) WITHOUT ROWID; INSERT INTO audio VALUES('cat',x'0102',x'0304')`)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestResumeAndAtomicInstall(t *testing.T) {
	data := audioFixture(t)
	var resumed atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		if strings.HasSuffix(req.URL.Path, ".sha256") {
			fmt.Fprintf(writer, "%x  audio-pack.db", sha256.Sum256(data))
			return
		}
		if req.Header.Get("Range") != "bytes=100-" {
			t.Errorf("range: %q", req.Header.Get("Range"))
		}
		resumed.Store(true)
		writer.Header().Set("Content-Range", fmt.Sprintf("bytes 100-%d/%d", len(data)-1, len(data)))
		writer.WriteHeader(206)
		writer.Write(data[100:])
	}))
	defer server.Close()
	target := filepath.Join(t.TempDir(), "audio-pack.db")
	cache := target + ".download-" + DataTag
	if err := os.WriteFile(cache+".part", data[:100], 0600); err != nil {
		t.Fatal(err)
	}
	downloader := &Downloader{Sources: []string{server.URL + "/"}}
	var phases []string
	err := downloader.Ensure(context.Background(), Audio, target, false, func(progress Progress) { phases = append(phases, progress.Phase) }, func(_ context.Context, stage string) error { return os.Rename(stage, target) })
	if err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(actual, data) || !resumed.Load() {
		t.Fatalf("install/resume: %v", err)
	}
	if phases[len(phases)-1] != "ready" {
		t.Fatal(phases)
	}
	if _, err := os.Stat(cache); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cache retained after success")
	}
	server.Close()
	if err := downloader.Ensure(context.Background(), Audio, target, false, nil, nil); err != nil {
		t.Fatal("installed asset used network:", err)
	}
}

func TestChecksumRetryAndPreserveInstalledData(t *testing.T) {
	data := audioFixture(t)
	var downloads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		if strings.HasSuffix(req.URL.Path, ".sha256") {
			fmt.Fprintf(writer, "%x", sha256.Sum256(data))
			return
		}
		downloads.Add(1)
		writer.Write([]byte("corrupt"))
	}))
	defer server.Close()
	target := filepath.Join(t.TempDir(), "audio-pack.db")
	os.WriteFile(target, data, 0600)
	downloader := &Downloader{Sources: []string{server.URL + "/"}}
	err := downloader.Ensure(context.Background(), Audio, target, true, nil, func(context.Context, string) error { t.Fatal("corrupt asset installed"); return nil })
	if !errors.Is(err, errChecksum) || downloads.Load() != 2 {
		t.Fatalf("retry: downloads=%d err=%v", downloads.Load(), err)
	}
	actual, _ := os.ReadFile(target)
	if !bytes.Equal(actual, data) {
		t.Fatal("old database changed")
	}
}

func TestRangeMismatchRestartsAndMirrorFallback(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		if strings.HasPrefix(req.URL.Path, "/bad/") {
			writer.WriteHeader(503)
			return
		}
		if requests.Add(1) == 1 {
			writer.Header().Set("Content-Range", "bytes 0-3/4")
			writer.WriteHeader(206)
			writer.Write([]byte("test"))
			return
		}
		if req.Header.Get("Range") != "" {
			t.Error("restart still has Range")
		}
		writer.Write([]byte("test"))
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "download")
	os.WriteFile(path+".part", []byte("xx"), 0600)
	downloader := &Downloader{Sources: []string{server.URL + "/bad/", server.URL + "/good/"}}
	if err := downloader.download(context.Background(), Audio, path, func(Progress) {}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "test" {
		t.Fatalf("corrupt append: %q", data)
	}
}

func TestCancellationKeepsPartialDownload(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		writer.Header().Set("Content-Length", "100")
		writer.Write([]byte("part"))
		writer.(http.Flusher).Flush()
		close(started)
		<-req.Context().Done()
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := filepath.Join(t.TempDir(), "download")
	downloader := &Downloader{Sources: []string{server.URL + "/"}}
	err := downloader.download(ctx, Audio, path, func(progress Progress) {
		if progress.Received > 0 {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path + ".part")
	if string(data) != "part" {
		t.Fatalf("partial lost: %q", data)
	}
}

func TestZIPSafetyAndCRC(t *testing.T) {
	for _, name := range []string{"../dictionary.db", "/dictionary.db", `C:\dictionary.db`, "safe/../../dictionary.db"} {
		t.Run(name, func(t *testing.T) {
			var buffer bytes.Buffer
			archive := zip.NewWriter(&buffer)
			entry, _ := archive.Create(name)
			entry.Write([]byte("test"))
			archive.Close()
			path := filepath.Join(t.TempDir(), "dictionary.zip")
			os.WriteFile(path, buffer.Bytes(), 0600)
			if err := extract(context.Background(), path, &bytes.Buffer{}); err == nil {
				t.Fatal("unsafe zip accepted")
			}
		})
	}
	var buffer bytes.Buffer
	archive := zip.NewWriter(&buffer)
	entry, _ := archive.CreateHeader(&zip.FileHeader{Name: "dictionary.db", Method: zip.Store})
	entry.Write([]byte("test-data"))
	archive.Close()
	data := buffer.Bytes()
	offset := bytes.Index(data, []byte("test-data"))
	data[offset] = 'X'
	path := filepath.Join(t.TempDir(), "dictionary.zip")
	os.WriteFile(path, data, 0600)
	if err := extract(context.Background(), path, &bytes.Buffer{}); !errors.Is(err, zip.ErrChecksum) {
		t.Fatalf("CRC: %v", err)
	}
}

func TestInstallFailureReusesDownload(t *testing.T) {
	data := audioFixture(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		if strings.HasSuffix(req.URL.Path, ".sha256") {
			writer.WriteHeader(404)
			return
		}
		requests.Add(1)
		writer.Write(data)
	}))
	defer server.Close()
	target := filepath.Join(t.TempDir(), "audio-pack.db")
	downloader := &Downloader{Sources: []string{server.URL + "/"}}
	denied := errors.New("installation denied")
	if err := downloader.Ensure(context.Background(), Audio, target, false, nil, func(context.Context, string) error { return denied }); !errors.Is(err, denied) {
		t.Fatal(err)
	}
	if err := downloader.Ensure(context.Background(), Audio, target, false, nil, func(_ context.Context, stage string) error { return os.Rename(stage, target) }); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 {
		t.Fatalf("downloaded %d times", requests.Load())
	}
}

func TestIdleBodyTimesOut(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		writer.WriteHeader(200)
		writer.(http.Flusher).Flush()
		<-req.Context().Done()
	}))
	defer server.Close()
	downloader := &Downloader{IdleTimeout: 20 * time.Millisecond}
	err := downloader.transfer(context.Background(), route{base: server.URL + "/"}, Audio, filepath.Join(t.TempDir(), "part"), func(Progress) {})
	if err == nil {
		t.Fatal("stalled response accepted")
	}
}

func TestForceReplacesOnlyAfterValidation(t *testing.T) {
	data := audioFixture(t)
	var downloads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		if strings.HasSuffix(req.URL.Path, ".sha256") {
			writer.WriteHeader(404)
			return
		}
		downloads.Add(1)
		writer.Write(data)
	}))
	defer server.Close()
	target := filepath.Join(t.TempDir(), "audio-pack.db")
	os.WriteFile(target, []byte("previous data"), 0600)
	downloader := &Downloader{Sources: []string{server.URL + "/"}}
	err := downloader.Ensure(context.Background(), Audio, target, true, nil, func(_ context.Context, stage string) error {
		old, _ := os.ReadFile(target)
		if string(old) != "previous data" {
			t.Error("old file touched before installation")
		}
		return os.Rename(stage, target)
	})
	if err != nil || downloads.Load() != 1 {
		t.Fatalf("force: %v downloads %d", err, downloads.Load())
	}
	installed, _ := os.ReadFile(target)
	if !bytes.Equal(installed, data) {
		t.Fatal("new data not installed")
	}
}

func TestConcurrentInstallerCannotTakeLock(t *testing.T) {
	target := filepath.Join(t.TempDir(), "dictionary.db")
	lock := flock.New(target + ".download.lock")
	if err := lock.Lock(); err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	err := (&Downloader{}).Ensure(context.Background(), Dictionary, target, false, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "another client") {
		t.Fatalf("lock ignored: %v", err)
	}
}

func TestInstalledAssetHasNoFilesystemSideEffects(t *testing.T) {
	target := filepath.Join(t.TempDir(), "dictionary.db")
	if err := os.WriteFile(target, []byte("existing"), 0400); err != nil {
		t.Fatal(err)
	}
	if err := (&Downloader{}).Ensure(context.Background(), Dictionary, target, false, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target + ".download.lock"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("created lock for installed data")
	}
}
