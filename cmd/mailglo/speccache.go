package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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

// A cache file is one line of JSON metadata, a newline, then the
// description exactly as the server sent it. Every description the CLI
// accepts therefore fits within the read limit.
const (
	maxMetaBytes  = 64 << 10
	maxCacheBytes = maxMetaBytes + maxSpecBytes
)

type cachedSpec struct {
	Server string `json:"server"`
	// URL is where the description was finally fetched from, after
	// same-origin redirects; relative server URLs resolve against it.
	URL          string    `json:"url"`
	ETag         string    `json:"etag,omitempty"`
	LastModified string    `json:"last_modified,omitempty"`
	FetchedAt    time.Time `json:"fetched_at"`
	Spec         []byte    `json:"-"`
	parsed       *api      // set once Spec has been parsed
}

// parseServer checks a server URL and returns it without a trailing slash.
func parseServer(s string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(s), "/"))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("server must be an http(s) URL without credentials, query or fragment, such as %s", defaultServer)
	}
	return u, nil
}

// specCache is the cache directory, opened once and verified through the
// open handle; every file operation is relative to that handle.
type specCache struct {
	root *os.Root
	name string
}

// openCache opens the cache for server, or returns nil when there is no
// cache directory this user alone controls.
func openCache(server string, stderr io.Writer) *specCache {
	base, err := os.UserCacheDir()
	if err != nil {
		return nil
	}
	dir := filepath.Join(base, "mailglo")
	refuse := func(err error) *specCache {
		warn(stderr, "warning: not using the cache: "+err.Error())
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return refuse(err)
	}
	before, err := os.Lstat(dir)
	if err != nil {
		return refuse(err)
	}
	if !before.IsDir() { // includes a symbolic link
		return refuse(fmt.Errorf("%s is not a directory", dir))
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return refuse(err)
	}
	d, err := root.Open(".")
	if err == nil {
		var fi os.FileInfo
		if fi, err = d.Stat(); err == nil {
			switch {
			case !os.SameFile(before, fi):
				err = fmt.Errorf("%s changed while it was opened", dir)
			default:
				err = verifyDir(d, fi, dir)
			}
		}
		_ = d.Close()
	}
	if err != nil {
		_ = root.Close()
		return refuse(err)
	}
	sum := sha256.Sum256([]byte(server))
	return &specCache{root: root, name: "spec-" + hex.EncodeToString(sum[:8]) + ".json"}
}

func (c *specCache) close() {
	if c != nil {
		_ = c.root.Close()
	}
}

// read returns the cached description for server. The entry must not be a
// link, and the opened file must be the same regular file, owned by this
// user and not writable by others.
func (c *specCache) read(server string, stderr io.Writer) *cachedSpec {
	if c == nil {
		return nil
	}
	ignore := func(err error) *cachedSpec {
		warn(stderr, fmt.Sprintf("warning: ignoring the cached API description %s: %v", c.name, err))
		return nil
	}
	entry, err := c.root.Lstat(c.name)
	if err != nil {
		return nil
	}
	if err := trustedFile(entry); err != nil {
		return ignore(err)
	}
	f, err := c.root.OpenFile(c.name, os.O_RDONLY|noFollow, 0)
	if err != nil {
		return ignore(err)
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return ignore(err)
	}
	if !os.SameFile(entry, fi) {
		return ignore(errors.New("the file changed while it was opened"))
	}
	if err := trustedFile(fi); err != nil {
		return ignore(err)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxCacheBytes+1))
	if err != nil {
		return ignore(err)
	}
	if len(b) > maxCacheBytes {
		return ignore(errors.New("the file is too large"))
	}
	meta, spec, ok := bytes.Cut(b, []byte("\n"))
	var cs cachedSpec
	if !ok || len(meta) > maxMetaBytes || json.Unmarshal(meta, &cs) != nil || cs.Server != server {
		return nil
	}
	cs.Spec = spec
	if u, err := url.Parse(cs.URL); err != nil || !sameOrigin(u, mustParse(server)) {
		return nil
	}
	return &cs
}

func (c *specCache) write(cs *cachedSpec) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(cs); err != nil { // one line, ending in a newline
		return err
	}
	if buf.Len() > maxMetaBytes {
		return errors.New("the response headers are too large to cache")
	}
	buf.Write(cs.Spec)
	b := buf.Bytes()
	tmp := ".spec-" + rand.Text()
	f, err := c.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|noFollow, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = c.root.Remove(tmp) }()
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return c.root.Rename(tmp, c.name)
}

// loadSpec returns the server's parsed API description and the URL it came
// from, or an exit code when there is none to use. The description is
// parsed once per run.
func loadSpec(base *url.URL, stderr io.Writer) (*api, string, int) {
	server := base.String()
	cache := openCache(server, stderr)
	defer cache.close()
	cached := cache.read(server, stderr)

	use, err := fetchSpec(server, cached)
	if err == nil && use.parsed != nil && cache != nil { // a new description
		if werr := cache.write(use); werr != nil {
			warn(stderr, "warning: cannot cache the API description: "+werr.Error())
		}
	} else if err == nil && use != cached && cache != nil { // the same one, moved
		_ = cache.write(use)
	}
	if err != nil {
		if cached == nil {
			warn(stderr, fmt.Sprintf("cannot fetch the API description from %s: %v; retry later", server, err))
			return nil, "", exitRetry
		}
		warn(stderr, fmt.Sprintf("warning: cannot fetch the API description from %s (%v); using the copy cached %s",
			server, err, cached.FetchedAt.Local().Format(time.RFC3339)))
		use = cached
	}
	if use.parsed == nil {
		a, perr := parseSpec(use.Spec)
		if perr != nil {
			warn(stderr, "the cached API description is not usable ("+perr.Error()+"); retry later")
			return nil, "", exitRetry
		}
		use.parsed = a
	}
	return use.parsed, use.URL, exitOK
}

// fetchSpec downloads the description, or returns cached when the server
// says it is still current. It never sends the API token, and it follows
// redirects only within the server's origin, so a redirect cannot swap in a
// description from elsewhere or downgrade https to http.
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
	// The description decides what later requests do, so it travels like
	// them: on the server's origin, and directly when that is this machine.
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: sameOriginRedirects, Transport: transport(mustParse(server))}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	final := resp.Request.URL.String()
	switch {
	case resp.StatusCode == http.StatusNotModified && cached != nil:
		if cached.URL == final {
			return cached, nil
		}
		moved := *cached
		moved.URL = final
		return &moved, nil
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
	a, err := parseSpec(b)
	if err != nil {
		return nil, fmt.Errorf("not a usable OpenAPI description: %w", err)
	}
	return &cachedSpec{Server: server, URL: final, ETag: resp.Header.Get("ETag"), LastModified: resp.Header.Get("Last-Modified"),
		FetchedAt: time.Now().UTC(), Spec: b, parsed: a}, nil
}

// sameOriginRedirects is an http.Client redirect policy that stays on the
// first request's scheme, host and port.
func sameOriginRedirects(r *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("too many redirects")
	}
	if !sameOrigin(r.URL, via[0].URL) {
		return fmt.Errorf("refusing to follow a redirect to another origin (%s)", origin(r.URL))
	}
	return nil
}

// sameOrigin compares scheme, host and effective port.
func sameOrigin(a, b *url.URL) bool { return origin(a) == origin(b) }

func origin(u *url.URL) string {
	scheme := strings.ToLower(u.Scheme)
	port := u.Port()
	if port == "" {
		switch scheme {
		case "https":
			port = "443"
		case "http":
			port = "80"
		}
	}
	host := strings.ToLower(u.Hostname())
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return scheme + "://" + host + ":" + port
}

func mustParse(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		return &url.URL{}
	}
	return u
}

func userAgent() string {
	v, _, _ := buildInfo()
	return "mailglo-cli/" + v
}
