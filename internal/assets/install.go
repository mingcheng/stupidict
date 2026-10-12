package assets

import (
	"archive/zip"
	"compress/flate"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	dictionary "github.com/mingcheng/stupidict"
	"github.com/gofrs/flock"
)

// Install is invoked with a verified database next to its destination. The
// caller owns closing live readers and atomically replacing the destination.
type Install func(context.Context, string) error

func (downloader *Downloader) Ensure(ctx context.Context, asset, target string, force bool, report Reporter, install Install) error {
	if report == nil {
		report = func(Progress) {}
	}
	if asset != Dictionary && asset != Audio {
		return errors.New("unsupported data asset")
	}
	// Reusing installed assets must work in read-only directories and must not
	// create locks, staging files or network requests merely to start the app.
	if !force {
		if info, err := os.Stat(target); err == nil && info.Mode().IsRegular() && info.Size() > 0 {
			report(Progress{Asset: asset, Phase: "ready"})
			return nil
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		return err
	}
	lock := flock.New(target + ".download.lock")
	locked, err := lock.TryLock()
	if err != nil {
		return err
	}
	if !locked {
		return errors.New("another client is downloading this asset")
	}
	defer lock.Close()
	if !force {
		if info, err := os.Stat(target); err == nil && info.Mode().IsRegular() && info.Size() > 0 {
			report(Progress{Asset: asset, Phase: "ready"})
			return nil
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	cache := target + ".download-" + DataTag
	report(Progress{Asset: asset, Phase: "metadata"})
	expected, err := downloader.checksum(ctx, asset)
	if err != nil {
		return err
	}
	for retry := 0; retry < 2; retry++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if _, err := os.Stat(cache); errors.Is(err, os.ErrNotExist) {
			if err := downloader.download(ctx, asset, cache, report); err != nil {
				return fmt.Errorf("download %s: %w", asset, err)
			}
		} else if err != nil {
			return err
		}
		report(Progress{Asset: asset, Phase: "verify"})
		err = verify(ctx, cache, expected)
		if err == nil {
			err = downloader.install(ctx, asset, cache, target, report, install)
		}
		if err == nil {
			_ = purge(cache)
			report(Progress{Asset: asset, Phase: "ready"})
			return nil
		}
		var corruptDeflate flate.CorruptInputError
		if errors.Is(err, errChecksum) || errors.Is(err, zip.ErrChecksum) || errors.Is(err, zip.ErrFormat) || errors.Is(err, errInvalidDatabase) || errors.Is(err, io.ErrUnexpectedEOF) || errors.As(err, &corruptDeflate) {
			if removeErr := purge(cache); removeErr != nil {
				return errors.Join(err, removeErr)
			}
			continue
		}
		return err
	}
	return fmt.Errorf("asset still corrupt after one fresh download: %w", err)
}

func purge(path string) error {
	for _, name := range []string{path, path + ".part"} {
		if err := os.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (downloader *Downloader) install(ctx context.Context, asset, cache, target string, report Reporter, install Install) error {
	file, err := os.CreateTemp(filepath.Dir(target), ".stupidict-install-*.db")
	if err != nil {
		return err
	}
	stage := file.Name()
	defer os.Remove(stage)
	if asset == Dictionary {
		report(Progress{Asset: asset, Phase: "extract"})
		err = extract(ctx, cache, file)
	} else {
		report(Progress{Asset: asset, Phase: "install"})
		var source *os.File
		source, err = os.Open(cache)
		if err == nil {
			_, err = copyContext(ctx, file, source)
			source.Close()
		}
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	if err = errors.Join(err, syncErr, closeErr); err != nil {
		return err
	}
	report(Progress{Asset: asset, Phase: "validate"})
	if err := validate(ctx, asset, stage); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return install(ctx, stage)
}

func extract(ctx context.Context, path string, target io.Writer) error {
	archive, err := zip.OpenReader(path)
	if err != nil {
		return err
	}
	defer archive.Close()
	var entry *zip.File
	for _, candidate := range archive.File {
		name := strings.ReplaceAll(candidate.Name, `\`, "/")
		if strings.HasPrefix(name, "/") || strings.Contains(name, ":") || candidate.Mode()&os.ModeSymlink != 0 {
			return errors.New("unsafe ZIP entry")
		}
		for _, part := range strings.Split(name, "/") {
			if part == ".." {
				return errors.New("unsafe ZIP traversal")
			}
		}
		if filepath.Base(name) == "dictionary.db" && !candidate.FileInfo().IsDir() {
			if entry != nil {
				return errors.New("ambiguous dictionary ZIP")
			}
			entry = candidate
		}
	}
	if entry == nil {
		return fmt.Errorf("%w: dictionary.db not found", zip.ErrFormat)
	}
	source, err := entry.Open()
	if err != nil {
		return err
	}
	defer source.Close()
	// archive/zip verifies CRC when the entry is read through EOF.
	_, err = copyContext(ctx, target, source)
	return err
}

var errInvalidDatabase = errors.New("invalid asset database")

func validate(ctx context.Context, asset, path string) error {
	// Asset opens are read-only and every connection is closed before rename.
	if asset == Dictionary {
		store, err := dictionary.Open(ctx, path)
		if err != nil {
			return fmt.Errorf("%w: %v", errInvalidDatabase, err)
		}
		store.Close()
	} else {
		store, err := dictionary.OpenAudio(ctx, path)
		if err != nil {
			return fmt.Errorf("%w: %v", errInvalidDatabase, err)
		}
		store.Close()
	}
	address := url.URL{Scheme: "file", Path: filepath.ToSlash(path), RawQuery: "mode=ro"}
	connection, err := sql.Open("sqlite", address.String())
	if err != nil {
		return err
	}
	defer connection.Close()
	connection.SetMaxOpenConns(1)
	var check string
	if err := connection.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&check); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: %v", errInvalidDatabase, err)
	}
	if check != "ok" {
		return fmt.Errorf("%w: %s", errInvalidDatabase, check)
	}
	table := "word"
	if asset == Audio {
		table = "audio"
	}
	var count int
	if err := connection.QueryRowContext(ctx, "SELECT 1 FROM "+table+" LIMIT 1").Scan(&count); err != nil {
		return fmt.Errorf("%w: empty %s", errInvalidDatabase, table)
	}
	return nil
}
