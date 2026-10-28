package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// call maps flags onto one operation, sends it and prints the response.
func call(base *url.URL, o *operation, args []string, token string, stdin io.Reader, stdout, stderr io.Writer) int {
	values := map[string]string{}
	var body []byte
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, val, inline := strings.Cut(strings.TrimPrefix(a, "--"), "=")
		if !strings.HasPrefix(a, "--") || name == "" || (!inline && i+1 >= len(args)) {
			return fail(stderr, exitUsage, fmt.Sprintf("expected --name value, got %q", a))
		}
		if !inline {
			val = args[i+1]
			i++
		}
		if name != "body" {
			values[name] = val
			continue
		}
		var err error
		switch {
		case val == "-":
			body, err = io.ReadAll(stdin)
		case strings.HasPrefix(val, "@"):
			body, err = os.ReadFile(strings.TrimPrefix(val, "@"))
		default:
			body = []byte(val)
		}
		if err != nil {
			return fail(stderr, exitUsage, "cannot read body: "+err.Error())
		}
		if !json.Valid(body) {
			return fail(stderr, exitUsage, "--body must be JSON")
		}
	}

	path := o.Path
	q := url.Values{}
	headers := http.Header{}
	for _, p := range o.Params {
		v, ok := values[p.Name]
		if !ok {
			if p.Required {
				return fail(stderr, exitUsage, fmt.Sprintf("missing required --%s; run: mailglo help %s", p.Name, kebab(o.ID)))
			}
			continue
		}
		delete(values, p.Name)
		switch p.In {
		case "path":
			path = strings.ReplaceAll(path, "{"+p.Name+"}", url.PathEscape(v))
		case "query":
			q.Set(p.Name, v)
		case "header":
			headers.Set(p.Name, v)
		}
	}
	if unknown := keys(boolSet(values)); len(unknown) > 0 {
		return fail(stderr, exitUsage, fmt.Sprintf("unknown flag --%s for %s; run: mailglo help %s", unknown[0], kebab(o.ID), kebab(o.ID)))
	}

	target := base.String() + "/api/v1" + path
	if len(q) > 0 {
		target += "?" + q.Encode()
	}
	req, err := http.NewRequest(o.Method, target, bytes.NewReader(body))
	if err != nil {
		return fail(stderr, exitError, err.Error())
	}
	req.Header = headers
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent())
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 60 * time.Second, CheckRedirect: func(r *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("too many redirects")
		}
		if token != "" && !safeForToken(r.URL) {
			return fmt.Errorf("refusing to follow a redirect to %s with MAILGLO_TOKEN", r.URL.Redacted())
		}
		return nil
	}}
	resp, err := client.Do(req)
	if err != nil {
		return fail(stderr, exitRetry, "request failed: "+err.Error())
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return fail(stderr, exitRetry, "reading the response failed: "+err.Error())
	}
	if len(bytes.TrimSpace(out)) == 0 {
		out = []byte(fmt.Sprintf(`{"status":%d}`, resp.StatusCode))
	}
	var pretty bytes.Buffer
	if json.Indent(&pretty, out, "", "  ") == nil {
		out = pretty.Bytes()
	}
	w := stdout
	if resp.StatusCode >= 400 {
		w = stderr
	}
	_, _ = fmt.Fprintln(w, string(out))

	// A partial list is never presented as complete.
	if resp.Header.Get("X-Result-Complete") == "false" {
		_, _ = fmt.Fprintf(stderr, "mailglo: incomplete result: repeat with --cursor %s for the next page\n", resp.Header.Get("X-Next-Cursor"))
	}
	if n := resp.Header.Get("X-Changed-Since-Snapshot"); n != "" && n != "0" {
		_, _ = fmt.Fprintf(stderr, "mailglo: %s item(s) changed since the first page; list again to see them\n", n)
	}
	if resp.Header.Get("Idempotent-Replay-Redacted") == "true" {
		_, _ = fmt.Fprintln(stderr, "mailglo: replayed result: issued secrets are not shown again")
	}
	return exitCode(resp.StatusCode)
}

// safeForToken reports whether a bearer token may be sent to u: over https,
// or over plain http only to this machine.
func safeForToken(u *url.URL) bool {
	if u.Scheme == "https" {
		return true
	}
	if u.Scheme != "http" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func exitCode(status int) int {
	switch {
	case status < 400:
		return exitOK
	case status == 401 || status == 403:
		return exitAuth
	case status == 404 || status == 410:
		return exitNotFound
	case status == 409 || status == 402:
		return exitConflict
	case status == 429 || status >= 500:
		return exitRetry
	}
	return exitError
}

func boolSet(m map[string]string) map[string]bool {
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}
