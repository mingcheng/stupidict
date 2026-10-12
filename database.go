package dictionary

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	"github.com/mingcheng/stupidict/internal/sqliteuri"
	_ "modernc.org/sqlite"
)

func openReadOnly(ctx context.Context, path string) (*sql.DB, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular database file: %s", path)
	}
	uri, err := sqliteuri.For(absolute, url.Values{"mode": {"ro"}})
	if err != nil {
		return nil, err
	}
	database, err := sql.Open("sqlite", uri)
	if err != nil {
		return nil, err
	}
	database.SetMaxOpenConns(4)
	if err := database.PingContext(ctx); err != nil {
		database.Close()
		return nil, err
	}
	return database, nil
}
