package sqliteuri

import (
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func TestFor(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "with space")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "data.db")
	uri, err := For(path, url.Values{"mode": {"rwc"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(uri, "file:///") || !strings.HasSuffix(uri, "/with%20space/data.db?mode=rwc") {
		t.Fatalf("URI %q", uri)
	}
	if runtime.GOOS == "windows" && !strings.HasPrefix(uri, "file:///"+filepath.VolumeName(path)) {
		t.Fatalf("drive letter not in path: %q", uri)
	}
	database, err := sql.Open("sqlite", uri)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.Exec("CREATE TABLE probe(id INTEGER)"); err != nil {
		t.Fatalf("open %q: %v", uri, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("database not created at %s: %v", path, err)
	}
}
