// Package sqliteuri builds SQLite "file:" URIs that open the same file on every
// platform.
package sqliteuri

import (
	"net/url"
	"path/filepath"
	"strings"
)

// For returns a file: URI for path with the given query parameters. Windows
// drive paths gain a leading slash ("file:///C:/..."); without it "C:" would be
// parsed as the URI authority and SQLite would refuse to open the file.
func For(path string, query url.Values) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	slashed := filepath.ToSlash(absolute)
	if !strings.HasPrefix(slashed, "/") {
		slashed = "/" + slashed
	}
	return (&url.URL{Scheme: "file", Path: slashed, RawQuery: query.Encode()}).String(), nil
}
