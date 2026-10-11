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
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mattn/go-ieproxy"
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

type route struct {
	base  string
	proxy func(*http.Request) (*url.URL, error)
}

// Downloader permits local HTTP servers in tests. Production uses the desktop's
// pinned data release and mirror order, then direct mirrors and local proxies.
type Downloader struct {
	Sources       []string
	IdleTimeout   time.Duration
	ProbeProxies  bool
	PlatformProxy func(*http.Request) (*url.URL, error)
}

func NewDownloader() *Downloader {
	base := "https://github.com/cholf5/stupid-dict/releases/download/" + DataTag + "/"
	return &Downloader{Sources: []string{base, "https://ghfast.top/" + base, "https://gh-proxy.com/" + base, "https://ghproxy.net/" + base}, IdleTimeout: 60 * time.Second, ProbeProxies: true,
		PlatformProxy: func(req *http.Request) (*url.URL, error) { return ieproxy.GetProxyFunc()(req) }}
}

func (downloader *Downloader) routes(ctx context.Context) func(func(route) bool) {
	return func(yield func(route) bool) {
		for _, base := range downloader.Sources {
			if !yield(route{base, downloader.PlatformProxy}) {
				return
			}
		}
		for _, base := range downloader.Sources[1:] {
			if !yield(route{base, nil}) {
				return
			}
		}
		if !downloader.ProbeProxies {
			return
		}
		for _, name := range []string{"HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy"} {
			proxy, err := url.Parse(os.Getenv(name))
			if err == nil && proxy.Host != "" {
				for _, base := range downloader.Sources[:min(2, len(downloader.Sources))] {
					if !yield(route{base, http.ProxyURL(proxy)}) {
						return
					}
				}
			}
		}
		for _, port := range []string{"7890", "7897", "1087", "10809", "6152", "8118", "1080"} {
			if ctx.Err() != nil {
				return
			}
			address := "127.0.0.1:" + port
			connection, err := (&net.Dialer{Timeout: 100 * time.Millisecond}).DialContext(ctx, "tcp", address)
			if err != nil {
				continue
			}
			connection.Close()
			scheme := "http"
			if port == "1080" {
				scheme = "socks5"
			}
			proxy, _ := url.Parse(scheme + "://" + address)
			for _, base := range downloader.Sources[:min(2, len(downloader.Sources))] {
				if !yield(route{base, http.ProxyURL(proxy)}) {
					return
				}
			}
		}
	}
}

type idleConnection struct {
	net.Conn
	timeout time.Duration
}

func (connection idleConnection) Read(buffer []byte) (int, error) {
	if err := connection.SetReadDeadline(time.Now().Add(connection.timeout)); err != nil {
		return 0, err
	}
	return connection.Conn.Read(buffer)
}

func (downloader *Downloader) client(attempt route) (*http.Client, *http.Transport) {
	idle := downloader.IdleTimeout
	if idle <= 0 {
		idle = 60 * time.Second
	}
	transport := &http.Transport{Proxy: attempt.proxy, ResponseHeaderTimeout: 15 * time.Second, TLSHandshakeTimeout: 10 * time.Second,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			connection, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, address)
			if err != nil {
				return nil, err
			}
			return idleConnection{connection, idle}, nil
		}}
	return &http.Client{Transport: transport}, transport
}

func request(ctx context.Context, address string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err == nil {
		req.Header.Set("User-Agent", "stupidict")
		req.Header.Set("Accept-Encoding", "identity")
	}
	return req, err
}

var contentRange = regexp.MustCompile(`^bytes ([0-9]+)-([0-9]+)/([0-9]+)$`)

func (downloader *Downloader) download(ctx context.Context, asset, path string, report Reporter) error {
	if len(downloader.Sources) == 0 {
		return errors.New("no download sources")
	}
	var last error
	for attempt := range downloader.routes(ctx) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		last = downloader.transfer(ctx, attempt, asset, path, report)
		if last == nil {
			return nil
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return fmt.Errorf("all download sources failed: %w", last)
}

func (downloader *Downloader) transfer(ctx context.Context, attempt route, asset, path string, report Reporter) error {
	client, transport := downloader.client(attempt)
	defer transport.CloseIdleConnections()
	part := path + ".part"
	for retry := 0; retry < 2; retry++ {
		var offset int64
		if info, err := os.Stat(part); err == nil {
			offset = info.Size()
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		req, err := request(ctx, attempt.base+asset)
		if err != nil {
			return err
		}
		if offset > 0 {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
		}
		report(Progress{Asset: asset, Phase: "download", Source: req.URL.String(), Received: offset, Resumed: offset, Total: -1})
		response, err := client.Do(req)
		if err != nil {
			return err
		}
		total := response.ContentLength
		if response.StatusCode == http.StatusRequestedRangeNotSatisfiable {
			response.Body.Close()
			if err := os.Remove(part); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			continue
		}
		if response.StatusCode == http.StatusPartialContent {
			match := contentRange.FindStringSubmatch(response.Header.Get("Content-Range"))
			valid := false
			if len(match) == 4 {
				start, startErr := strconv.ParseInt(match[1], 10, 64)
				end, endErr := strconv.ParseInt(match[2], 10, 64)
				size, sizeErr := strconv.ParseInt(match[3], 10, 64)
				valid = startErr == nil && endErr == nil && sizeErr == nil && start == offset && end >= start && size > end && end == size-1 && (response.ContentLength < 0 || response.ContentLength == end-start+1)
				total = size
			}
			if !valid {
				response.Body.Close()
				if err := os.Remove(part); err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
				continue
			}
		} else if response.StatusCode == http.StatusOK {
			offset = 0
		} else {
			response.Body.Close()
			return fmt.Errorf("HTTP %d", response.StatusCode)
		}
		flags := os.O_CREATE | os.O_WRONLY
		if offset > 0 {
			flags |= os.O_APPEND
		} else {
			flags |= os.O_TRUNC
		}
		file, err := os.OpenFile(part, flags, 0600)
		if err != nil {
			response.Body.Close()
			return err
		}
		progress := Progress{Asset: asset, Phase: "download", Source: req.URL.String(), Received: offset, Total: total, Resumed: offset}
		lastReport := time.Time{}
		_, copyErr := copyContext(ctx, file, response.Body, func(count int64) {
			progress.Received = offset + count
			if time.Since(lastReport) >= 100*time.Millisecond {
				report(progress)
				lastReport = time.Now()
			}
		})
		bodyErr := response.Body.Close()
		syncErr := file.Sync()
		closeErr := file.Close()
		if err := errors.Join(copyErr, bodyErr, syncErr, closeErr); err != nil {
			return err
		}
		if total >= 0 && progress.Received != total {
			return io.ErrUnexpectedEOF
		}
		report(progress)
		return os.Rename(part, path)
	}
	return errors.New("invalid or unsatisfiable Content-Range")
}

func copyContext(ctx context.Context, target io.Writer, source io.Reader, progress func(int64)) (int64, error) {
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
			if progress != nil {
				progress(total)
			}
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
	for _, base := range downloader.Sources {
		checksumCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		client, transport := downloader.client(route{base, downloader.PlatformProxy})
		req, err := request(checksumCtx, base+asset+".sha256")
		if err != nil {
			cancel()
			return "", err
		}
		response, err := client.Do(req)
		var data []byte
		if err == nil {
			if response.StatusCode == http.StatusOK {
				data, err = io.ReadAll(io.LimitReader(response.Body, 4097))
			}
			response.Body.Close()
		}
		cancel()
		transport.CloseIdleConnections()
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
	if _, err := copyContext(ctx, hash, file, nil); err != nil {
		return err
	}
	if hex.EncodeToString(hash.Sum(nil)) != expected {
		return errChecksum
	}
	return nil
}
