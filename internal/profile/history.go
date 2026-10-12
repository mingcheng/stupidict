package profile

import (
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mingcheng/stupidict/internal/sqliteuri"
	_ "modernc.org/sqlite"
)

type Record struct {
	ID      int64
	Query   string
	Updated time.Time
}

type History struct{ database *sql.DB }

// OpenHistory creates an independent writable SQLite store. Transactions keep
// visits atomic across clients; read-only asset connections remain untouched.
func OpenHistory(path string) (*History, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	address, err := sqliteuri.For(path, url.Values{"_pragma": {"busy_timeout(5000)"}})
	if err != nil {
		return nil, err
	}
	database, err := sql.Open("sqlite", address)
	if err != nil {
		return nil, err
	}
	database.SetMaxOpenConns(1)
	_, err = database.Exec(`CREATE TABLE IF NOT EXISTS recent_query (
	 id INTEGER PRIMARY KEY, query TEXT NOT NULL UNIQUE, updated INTEGER NOT NULL)`)
	if err != nil {
		database.Close()
		return nil, err
	}
	return &History{database: database}, nil
}

func (history *History) Close() error { return history.database.Close() }

func validQuery(query string) (string, error) {
	query = strings.TrimSpace(query)
	if query == "" || len(query) > 4096 {
		return "", errors.New("query must contain 1 to 4096 bytes")
	}
	return query, nil
}

func (history *History) Visit(query string) error {
	query, err := validQuery(query)
	if err != nil {
		return err
	}
	transaction, err := history.database.Begin()
	if err != nil {
		return err
	}
	defer transaction.Rollback()
	_, err = transaction.Exec(`INSERT INTO recent_query(query, updated) VALUES(?, ?)
	 ON CONFLICT(query) DO UPDATE SET updated=excluded.updated`, query, time.Now().UnixNano())
	if err != nil {
		return err
	}
	_, err = transaction.Exec(`DELETE FROM recent_query WHERE id NOT IN (SELECT id FROM recent_query ORDER BY updated DESC, id DESC LIMIT 30)`)
	if err != nil {
		return err
	}
	return transaction.Commit()
}

func (history *History) List() ([]Record, error) {
	rows, err := history.database.Query("SELECT id, query, updated FROM recent_query ORDER BY updated DESC, id DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := []Record{}
	for rows.Next() {
		var record Record
		var updated int64
		if err := rows.Scan(&record.ID, &record.Query, &updated); err != nil {
			return nil, err
		}
		record.Updated = time.Unix(0, updated)
		records = append(records, record)
	}
	return records, rows.Err()
}

func (history *History) Edit(id int64, query string) error {
	query, err := validQuery(query)
	if err != nil {
		return err
	}
	result, err := history.database.Exec("UPDATE recent_query SET query=?, updated=? WHERE id=?", query, time.Now().UnixNano(), id)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err == nil && count == 0 {
		return errors.New("record no longer exists")
	}
	return err
}

func (history *History) Delete(id int64) error {
	_, err := history.database.Exec("DELETE FROM recent_query WHERE id=?", id)
	return err
}

func (history *History) Clear() error {
	_, err := history.database.Exec("DELETE FROM recent_query")
	return err
}
