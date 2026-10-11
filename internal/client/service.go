// Package client coordinates read-only dictionary engines and local user state.
// It has no terminal dependencies and can serve another frontend unchanged.
package client

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	dictionary "github.com/mingcheng/stupidict"
	"github.com/mingcheng/stupidict/internal/assets"
	"github.com/mingcheng/stupidict/internal/profile"
)

type Engine interface {
	Lookup(context.Context, string) (dictionary.LookupResult, error)
	Suggest(context.Context, string) ([]string, error)
	Close() error
}

type Factory func(context.Context, string) (Engine, error)

type FileInfo struct {
	Size     int64
	Modified time.Time
	Err      error
}

type DataInfo struct {
	Entries    int64
	CountError error
	Dictionary FileInfo
	Audio      FileInfo
}

type Service struct {
	gate    sync.RWMutex
	config  profile.Config
	path    string
	engine  Engine
	openErr error
	factory Factory
	history *profile.History
}

func Open(ctx context.Context, path string, config profile.Config, factory Factory) (*Service, error) {
	config, err := config.Normalize(path)
	if err != nil {
		return nil, err
	}
	if factory == nil {
		factory = func(ctx context.Context, path string) (Engine, error) {
			engine, err := dictionary.Open(ctx, path)
			if err != nil {
				return nil, err
			}
			return engine, nil
		}
	}
	history, err := profile.OpenHistory(filepath.Join(filepath.Dir(path), "history.db"))
	if err != nil {
		return nil, err
	}
	engine, openErr := factory(ctx, config.Dictionary)
	// Missing assets do not prevent opening Settings to repair the path.
	return &Service{config: config, path: path, engine: engine, openErr: openErr, factory: factory, history: history}, nil
}

func (service *Service) Config() profile.Config {
	service.gate.RLock()
	defer service.gate.RUnlock()
	return service.config
}

func (service *Service) DataInfo(ctx context.Context) DataInfo {
	service.gate.RLock()
	defer service.gate.RUnlock()
	fileInfo := func(path string) FileInfo {
		info, err := os.Stat(path)
		if err != nil {
			return FileInfo{Err: err}
		}
		if !info.Mode().IsRegular() {
			return FileInfo{Err: errors.New("not a regular data file")}
		}
		return FileInfo{Size: info.Size(), Modified: info.ModTime()}
	}
	info := DataInfo{Dictionary: fileInfo(service.config.Dictionary), Audio: fileInfo(service.config.Audio)}
	if service.engine == nil {
		info.CountError = service.openErr
	} else if counter, ok := service.engine.(interface {
		EntryCount(context.Context) (int64, error)
	}); ok {
		info.Entries, info.CountError = counter.EntryCount(ctx)
	} else {
		info.CountError = errors.New("entry count unavailable")
	}
	return info
}

func (service *Service) Lookup(ctx context.Context, query string) (dictionary.LookupResult, error) {
	service.gate.RLock()
	defer service.gate.RUnlock()
	if service.engine == nil {
		return dictionary.LookupResult{}, service.openErr
	}
	return service.engine.Lookup(ctx, query)
}

func (service *Service) Suggest(ctx context.Context, query string) ([]string, error) {
	service.gate.RLock()
	defer service.gate.RUnlock()
	if service.engine == nil {
		return nil, service.openErr
	}
	return service.engine.Suggest(ctx, query)
}

// Accept is deliberately separate from Lookup: stale asynchronous results must
// not enter history. Navigation replay does not call this method either.
func (service *Service) Accept(result dictionary.LookupResult) error {
	if result.Primary == nil {
		return nil
	}
	return service.history.Visit(result.Query)
}

func (service *Service) History() ([]profile.Record, error) { return service.history.List() }
func (service *Service) EditHistory(id int64, query string) error {
	return service.history.Edit(id, query)
}
func (service *Service) DeleteHistory(id int64) error { return service.history.Delete(id) }
func (service *Service) ClearHistory() error          { return service.history.Clear() }

// Configure validates a replacement before saving or swapping engines. Holding
// the lock also prevents closing a connection underneath an in-flight query.
func (service *Service) Configure(ctx context.Context, config profile.Config) error {
	config, err := config.Normalize(service.path)
	if err != nil {
		return err
	}
	service.gate.Lock()
	defer service.gate.Unlock()
	var replacement Engine
	if config.Dictionary != service.config.Dictionary || service.engine == nil {
		replacement, err = service.factory(ctx, config.Dictionary)
		if err != nil {
			return err
		}
	}
	if err := profile.Save(service.path, config); err != nil {
		if replacement != nil {
			replacement.Close()
		}
		return err
	}
	if replacement != nil {
		if service.engine != nil {
			service.engine.Close()
		}
		service.engine, service.openErr = replacement, nil
	}
	service.config = config
	return nil
}

func (service *Service) Audio(ctx context.Context, word string, accent dictionary.Accent) ([]byte, error) {
	service.gate.RLock()
	defer service.gate.RUnlock()
	store, err := dictionary.OpenAudio(ctx, service.config.Audio)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	return store.GetAudio(ctx, word, accent)
}

func (service *Service) Ready() bool {
	service.gate.RLock()
	defer service.gate.RUnlock()
	return service.engine != nil
}

// Initialize installs the dictionary first, making it available while the
// optional speech database is still downloading. Only this method uses HTTP.
func (service *Service) Initialize(ctx context.Context, downloader *assets.Downloader, force bool, report assets.Reporter) error {
	config := service.Config()
	if !force && !service.Ready() {
		if info, err := os.Stat(config.Dictionary); err == nil && info.Size() > 0 {
			return errors.New("existing dictionary could not be opened; check its path or explicitly use -update-data")
		}
	}
	if err := downloader.Ensure(ctx, assets.Dictionary, config.Dictionary, force, report, func(ctx context.Context, stage string) error {
		service.gate.Lock()
		defer service.gate.Unlock()
		if service.config.Dictionary != config.Dictionary {
			return errors.New("dictionary path changed during download")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if service.engine != nil {
			if err := service.engine.Close(); err != nil {
				return err
			}
			service.engine = nil
		}
		moveErr := os.Rename(stage, config.Dictionary)
		// Once replacement begins, always reopen whichever complete file remains,
		// even if cancellation arrives between the rename and the factory call.
		reopenCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		service.engine, service.openErr = service.factory(reopenCtx, config.Dictionary)
		return errors.Join(moveErr, service.openErr)
	}); err != nil {
		return err
	}
	if config.Audio == "" {
		return nil
	}
	return downloader.Ensure(ctx, assets.Audio, config.Audio, force, report, func(ctx context.Context, stage string) error {
		service.gate.Lock()
		defer service.gate.Unlock()
		if service.config.Audio != config.Audio {
			return errors.New("audio path changed during download")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return os.Rename(stage, config.Audio)
	})
}

func (service *Service) Close() error {
	service.gate.Lock()
	defer service.gate.Unlock()
	var err error
	if service.engine != nil {
		err = service.engine.Close()
		service.engine = nil
	}
	service.openErr = errors.New("client is closed")
	return errors.Join(err, service.history.Close())
}
