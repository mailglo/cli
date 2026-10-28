package main

import (
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

const maxSpecBytes = 64 << 20

type cachedSpec struct {
	Server string `json:"server"`
	// URL is where the description was finally fetched from, after
	// same-origin redirects; relative server URLs resolve against it.
	URL          string          `json:"url"`
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
	b, err := io.ReadAll(io.LimitReader(f, 2*maxSpecBytes))
	if err != nil {
		return ignore(err)
	}
	var cs cachedSpec
	if json.Unmarshal(b, &cs) != nil || cs.Server != server || validSpec(cs.Spec) != nil {
		return nil
	}
	if u, err := url.Parse(cs.URL); err != nil || !sameOrigin(u, mustParse(server)) {
		return nil
	}
	return &cs
}

func (c *specCache) write(cs *cachedSpec) error {
	b, err := json.Marshal(cs)
	if err != nil {
		return err
	}
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

// loadSpec returns the server's API description and the URL it came from,
// or an exit code when there is none to use.
func loadSpec(base *url.URL, stderr io.Writer) ([]byte, string, int) {
	server := base.String()
	cache := openCache(server, stderr)
	defer cache.close()
	cached := cache.read(server, stderr)

	fresh, err := fetchSpec(server, cached)
	if err == nil {
		if fresh != cached && cache != nil {
			if werr := cache.write(fresh); werr != nil {
				warn(stderr, "warning: cannot cache the API description: "+werr.Error())
			}
		}
		return fresh.Spec, fresh.URL, exitOK
	}
	if cached != nil {
		warn(stderr, fmt.Sprintf("warning: cannot fetch the API description from %s (%v); using the copy cached %s",
			server, err, cached.FetchedAt.Local().Format(time.RFC3339)))
		return cached.Spec, cached.URL, exitOK
	}
	warn(stderr, fmt.Sprintf("cannot fetch the API description from %s: %v; retry later", server, err))
	return nil, "", exitRetry
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
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: sameOriginRedirects}
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
	if err := validSpec(b); err != nil {
		return nil, err
	}
	return &cachedSpec{Server: server, URL: final, ETag: resp.Header.Get("ETag"), LastModified: resp.Header.Get("Last-Modified"),
		FetchedAt: time.Now().UTC(), Spec: b}, nil
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
