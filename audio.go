package dictionary

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

type Accent string

const (
	British  Accent = "uk"
	American Accent = "us"
)

var ErrAudioNotFound = errors.New("audio not found")

type AudioStore struct {
	database *sql.DB
}

func OpenAudio(ctx context.Context, path string) (*AudioStore, error) {
	database, err := openReadOnly(ctx, path)
	if err != nil {
		return nil, err
	}
	return &AudioStore{database: database}, nil
}

func (store *AudioStore) Close() error { return store.database.Close() }

func (store *AudioStore) GetAudio(ctx context.Context, word string, accent Accent) ([]byte, error) {
	if accent != British && accent != American {
		return nil, fmt.Errorf("unsupported accent: %q", accent)
	}
	var audio []byte
	err := store.database.QueryRowContext(ctx,
		"SELECT "+string(accent)+" FROM audio WHERE word = ?", strings.ToLower(word)).Scan(&audio)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && len(audio) == 0) {
		return nil, ErrAudioNotFound
	}
	return audio, err
}
