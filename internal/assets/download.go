// Package assets bootstraps versioned offline data; lookup never uses the network.
package assets

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/cavaliergopher/grab/v3"
)

const DataTag = "data-3"
const ReleasePage = "https://github.com/cholf5/stupid-dict/releases/tag/" + DataTag
const Dictionary = "dictionary.zip"
const Audio = "audio-pack.db"

type Progress struct {
	Asset, Phase, Source     string
	Received, Total, Resumed int64
}

type Reporter func(Progress)

// Downloader permits local HTTP servers in tests. Production tries the pinned
// data release on GitHub first, then the mirrors in order.
type Downloader struct {
	Sources     []string
	IdleTimeout time.Duration
}

func NewDownloader() *Downloader {
	base := "https://github.com/cholf5/stupid-dict/releases/download/" + DataTag + "/"
	return &Downloader{Sources: []string{base, "https://ghfast.top/" + base, "https://gh-proxy.com/" + base, "https://ghproxy.net/" + base}, IdleTimeout: 60 * time.Second}
}

func transport() *http.Transport {
	return &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		DisableCompression:    true,
	}
}

func (downloader *Downloader) download(ctx context.Context, asset, path string, report Reporter) error {
	if len(downloader.Sources) == 0 {
		return errors.New("no download sources")
	}
	client := &grab.Client{UserAgent: "stupidict", HTTPClient: &http.Client{Transport: transport()}}
	var last error
	for _, base := range downloader.Sources {
		if err := ctx.Err(); err != nil {
			return err
		}
		last = downloader.fetch(ctx, client, base+asset, path, asset, report)
		if errors.Is(last, grab.ErrBadLength) {
			// A partial file larger than the remote asset cannot be resumed.
			if err := os.Remove(path + ".part"); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			last = downloader.fetch(ctx, client, base+asset, path, asset, report)
		}
		if last == nil {
			return nil
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return fmt.Errorf("all download sources failed: %w", last)
}

// fetch downloads address into path+".part", resuming when the server supports
// ranges, and renames it to path once complete. A transfer that receives no
// data for IdleTimeout is abandoned so the next source can be tried.
func (downloader *Downloader) fetch(ctx context.Context, client *grab.Client, address, path, asset string, report Reporter) error {
	part := path + ".part"
	var existing int64
	if info, err := os.Stat(part); err == nil {
		existing = info.Size()
	}
	request, err := grab.NewRequest(part, address)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	request = request.WithContext(ctx)
	// Keep local modification times; About reports them as such.
	request.IgnoreRemoteTime = true
	progress := Progress{Asset: asset, Phase: "download", Source: address, Received: existing, Total: -1}
	report(progress)
	response := client.Do(request)
	if response.DidResume {
		progress.Resumed = existing
	}
	idle := downloader.IdleTimeout
	if idle <= 0 {
		idle = 60 * time.Second
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	lastReceived, lastChange := int64(-1), time.Now()
	for {
		select {
		case <-response.Done:
			if err := response.Err(); err != nil {
				return err
			}
			progress.Received, progress.Total = response.BytesComplete(), response.Size()
			report(progress)
			return os.Rename(part, path)
		case now := <-ticker.C:
			progress.Received, progress.Total = response.BytesComplete(), response.Size()
			if progress.Received != lastReceived {
				lastReceived, lastChange = progress.Received, now
			} else if now.Sub(lastChange) >= idle {
				cancel()
				<-response.Done
				return fmt.Errorf("no data received for %s", idle)
			}
			report(progress)
		}
	}
}

func copyContext(ctx context.Context, target io.Writer, source io.Reader) (int64, error) {
	buffer := make([]byte, 64*1024)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		count, readErr := source.Read(buffer)
		if count > 0 {
			written, err := target.Write(buffer[:count])
			total += int64(written)
			if err != nil {
				return total, err
			}
			if written != count {
				return total, io.ErrShortWrite
			}
		}
		if readErr == io.EOF {
			return total, nil
		}
		if readErr != nil {
			return total, readErr
		}
	}
}

func (downloader *Downloader) checksum(ctx context.Context, asset string) (string, error) {
	// A missing checksum is allowed by the desktop protocol. Invalid published
	// checksums are not treated as absence, and cancellation always propagates.
	client := &http.Client{Transport: transport()}
	defer client.CloseIdleConnections()
	for _, base := range downloader.Sources {
		checksumCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		req, err := http.NewRequestWithContext(checksumCtx, http.MethodGet, base+asset+".sha256", nil)
		if err != nil {
			cancel()
			return "", err
		}
		req.Header.Set("User-Agent", "stupidict")
		response, err := client.Do(req)
		var data []byte
		if err == nil {
			if response.StatusCode == http.StatusOK {
				data, err = io.ReadAll(io.LimitReader(response.Body, 4097))
			}
			response.Body.Close()
		}
		cancel()
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if err != nil || len(data) == 0 {
			continue
		}
		fields := strings.Fields(string(data))
		if len(data) > 4096 || len(fields) == 0 || len(fields[0]) != 64 {
			return "", errors.New("invalid published SHA-256")
		}
		if _, err := hex.DecodeString(fields[0]); err != nil {
			return "", err
		}
		return strings.ToLower(fields[0]), nil
	}
	return "", nil
}

var errChecksum = errors.New("SHA-256 mismatch")

func verify(ctx context.Context, path, expected string) error {
	if expected == "" {
		return ctx.Err()
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := copyContext(ctx, hash, file); err != nil {
		return err
	}
	if hex.EncodeToString(hash.Sum(nil)) != expected {
		return errChecksum
	}
	return nil
}
