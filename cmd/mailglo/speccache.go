package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The CLI reads its commands from the server's API description at
// <server>/api/openapi.json. Each server's copy is cached in the user cache
// directory and revalidated with If-None-Match and If-Modified-Since, so a
// new server release shows up without a CLI upgrade. When the server cannot
// be reached, the cached copy is used with a warning.

const maxSpecBytes = 64 << 20

type cachedSpec struct {
	Server       string          `json:"server"`
	ETag         string          `json:"etag,omitempty"`
	LastModified string          `json:"last_modified,omitempty"`
	FetchedAt    time.Time       `json:"fetched_at"`
	Spec         json.RawMessage `json:"spec"`
}

// parseServer checks a server URL and returns it without a trailing slash.
func parseServer(s string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(s), "/"))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("server must be an http(s) URL without credentials, query or fragment, such as %s", defaultServer)
	}
	return u, nil
}

func cacheFile(server string) string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	sum := sha256.Sum256([]byte(server))
	return filepath.Join(dir, "mailglo", "spec-"+hex.EncodeToString(sum[:8])+".json")
}

func readCache(path, server string) *cachedSpec {
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var c cachedSpec
	if json.Unmarshal(b, &c) != nil || c.Server != server || validSpec(c.Spec) != nil {
		return nil
	}
	return &c
}

func writeCache(path string, c *cachedSpec) error {
	if path == "" {
		return fmt.Errorf("no user cache directory")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".spec-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// loadSpec returns the server's API description, or an exit code when there
// is none to use.
func loadSpec(base *url.URL, stderr io.Writer) ([]byte, int) {
	server := base.String()
	path := cacheFile(server)
	cached := readCache(path, server)

	fresh, err := fetchSpec(server, cached)
	if err == nil && fresh == nil {
		return cached.Spec, exitOK // 304 Not Modified
	}
	if err == nil {
		if werr := writeCache(path, fresh); werr != nil {
			_, _ = fmt.Fprintln(stderr, "mailglo: warning: cannot cache the API description:", werr)
		}
		return fresh.Spec, exitOK
	}
	if cached != nil {
		_, _ = fmt.Fprintf(stderr, "mailglo: warning: cannot fetch the API description from %s (%v); using the copy cached %s\n",
			server, err, cached.FetchedAt.Local().Format(time.RFC3339))
		return cached.Spec, exitOK
	}
	_, _ = fmt.Fprintf(stderr, "mailglo: cannot fetch the API description from %s: %v; retry later\n", server, err)
	return nil, exitRetry
}

// fetchSpec downloads the description. It returns nil, nil when the cached
// copy is still current. It never sends the API token.
func fetchSpec(server string, cached *cachedSpec) (*cachedSpec, error) {
	req, err := http.NewRequest(http.MethodGet, server+"/api/openapi.json", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent())
	if cached != nil {
		if cached.ETag != "" {
			req.Header.Set("If-None-Match", cached.ETag)
		}
		if cached.LastModified != "" {
			req.Header.Set("If-Modified-Since", cached.LastModified)
		}
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusNotModified && cached != nil:
		return nil, nil
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxSpecBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxSpecBytes {
		return nil, fmt.Errorf("description larger than %d bytes", maxSpecBytes)
	}
	if err := validSpec(b); err != nil {
		return nil, err
	}
	return &cachedSpec{Server: server, ETag: resp.Header.Get("ETag"), LastModified: resp.Header.Get("Last-Modified"),
		FetchedAt: time.Now().UTC(), Spec: b}, nil
}

func userAgent() string {
	v, _, _ := buildInfo()
	return "mailglo-cli/" + v
}
