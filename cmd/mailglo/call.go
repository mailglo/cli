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

// Flags for an operation:
//
//	--<name> value          a parameter, when only one has that name
//	--<in>.<name> value     a parameter in path, query, header or cookie
//	--body JSON|@file|-     the request body, unless a parameter is called body
//	--request-body ...      the request body, always
//
// A flag for an array parameter can be repeated.

var locations = []string{"path", "query", "header", "cookie"}

// paramFlag is the flag help shows for p: the bare name when it is
// unambiguous, else the location-qualified one.
func paramFlag(o *operation, p param) string {
	if p.Name == "body" || p.Name == "request-body" || len(paramsNamed(o, p.Name)) > 1 {
		return "--" + p.In + "." + p.Name
	}
	return "--" + p.Name
}

func bodyFlag(o *operation) string {
	if len(paramsNamed(o, "body")) > 0 {
		return "--request-body"
	}
	return "--body"
}

func paramsNamed(o *operation, name string) []int {
	var out []int
	for i, p := range o.Params {
		if p.Name == name {
			out = append(out, i)
		}
	}
	return out
}

// resolveFlag maps a flag name to a parameter index, or to the body (-1).
func resolveFlag(o *operation, key string) (int, error) {
	switch {
	case key == "request-body":
		return -1, nil
	case key == "body":
		if len(paramsNamed(o, "body")) > 0 {
			return 0, fmt.Errorf("--body is ambiguous for %s: use --request-body for the request body or --<in>.body for the parameter", o.Name)
		}
		return -1, nil
	}
	if in, name, ok := strings.Cut(key, "."); ok {
		for _, loc := range locations {
			if in == loc {
				for i, p := range o.Params {
					if p.In == in && p.Name == name {
						return i, nil
					}
				}
			}
		}
	}
	switch idx := paramsNamed(o, key); len(idx) {
	case 0:
		return 0, fmt.Errorf("unknown flag --%s for %s; run: mailglo help %s", key, o.Name, o.Name)
	case 1:
		return idx[0], nil
	default:
		var flags []string
		for _, i := range idx {
			flags = append(flags, "--"+o.Params[i].In+"."+key)
		}
		return 0, fmt.Errorf("--%s is ambiguous for %s: use %s", key, o.Name, strings.Join(flags, " or "))
	}
}

// parseFlags reads an operation's flags into values per parameter and the body.
func parseFlags(o *operation, args []string, stdin io.Reader) (map[int][]string, []byte, error) {
	values := map[int][]string{}
	var body []byte
	for i := 0; i < len(args); i++ {
		a := args[i]
		key, val, inline := strings.Cut(strings.TrimPrefix(a, "--"), "=")
		if !strings.HasPrefix(a, "--") || key == "" || (!inline && i+1 >= len(args)) {
			return nil, nil, fmt.Errorf("expected --name value, got %q", a)
		}
		if !inline {
			val = args[i+1]
			i++
		}
		idx, err := resolveFlag(o, key)
		if err != nil {
			return nil, nil, err
		}
		if idx >= 0 {
			p := o.Params[idx]
			if len(values[idx]) > 0 && !p.Array {
				return nil, nil, fmt.Errorf("--%s is given more than once, but %s takes one value", key, p.Name)
			}
			values[idx] = append(values[idx], val)
			continue
		}
		if body != nil {
			return nil, nil, errors.New("the request body is given more than once")
		}
		switch {
		case val == "-":
			body, err = io.ReadAll(stdin)
		case strings.HasPrefix(val, "@"):
			body, err = os.ReadFile(strings.TrimPrefix(val, "@"))
		default:
			body = []byte(val)
		}
		if err != nil {
			return nil, nil, fmt.Errorf("cannot read body: %w", err)
		}
		if !json.Valid(body) {
			return nil, nil, fmt.Errorf("%s must be JSON", bodyFlag(o))
		}
	}
	for i, p := range o.Params {
		if p.Required && len(values[i]) == 0 {
			return nil, nil, fmt.Errorf("missing required %s; run: mailglo help %s", paramFlag(o, p), o.Name)
		}
	}
	switch {
	case body != nil && o.Body == nil:
		return nil, nil, fmt.Errorf("%s takes no request body", o.Name)
	case body != nil && o.Body.MediaType == "":
		return nil, nil, fmt.Errorf("%s takes a body as %s, which this CLI cannot send", o.Name, strings.Join(o.Body.Declared, " or "))
	case body == nil && o.Body != nil && o.Body.Required:
		return nil, nil, fmt.Errorf("%s needs a request body: %s JSON|@file|-", o.Name, bodyFlag(o))
	}
	return values, body, nil
}

// apiBase is the URL operation paths are appended to. The description's
// first server URL is resolved against the description's own URL, but only
// its path is used: requests, and the token, stay on the selected server's
// origin. Without "servers" the base is <server>/api/v1.
func apiBase(base *url.URL, a *api) string {
	if a.ServerURL != "" {
		doc, err1 := url.Parse(base.String() + "/api/openapi.json")
		ref, err2 := url.Parse(a.ServerURL)
		if err1 == nil && err2 == nil {
			u := doc.ResolveReference(ref)
			return base.Scheme + "://" + base.Host + strings.TrimRight(u.EscapedPath(), "/")
		}
	}
	return base.String() + "/api/v1"
}

// buildRequest serializes the parameters into path, query, headers and
// cookies.
func buildRequest(o *operation, target string, values map[int][]string) (string, http.Header, error) {
	path := o.Path
	var query, cookies []string
	headers := http.Header{}
	for i, p := range o.Params {
		vals := values[i]
		if len(vals) == 0 {
			continue
		}
		for _, v := range vals {
			if hasControl(v) {
				return "", nil, fmt.Errorf("--%s contains a control character", p.Name)
			}
		}
		switch p.In {
		case "path":
			var parts []string
			for _, v := range vals {
				// A dot segment or a slash would make the server route the
				// request to a different path, even escaped.
				if v == "" || v == "." || v == ".." || strings.ContainsAny(v, `/\`) {
					return "", nil, fmt.Errorf("--%s must not be empty, '.' or '..', or contain '/' or '\\'", p.Name)
				}
				parts = append(parts, url.PathEscape(v))
			}
			path = strings.ReplaceAll(path, "{"+p.Name+"}", strings.Join(parts, ","))
		case "query":
			name := url.QueryEscape(p.Name)
			var esc []string
			for _, v := range vals {
				esc = append(esc, url.QueryEscape(v))
			}
			switch {
			case !p.Array:
				query = append(query, name+"="+esc[0])
			case p.Style == "form" && p.Explode:
				for _, v := range esc {
					query = append(query, name+"="+v)
				}
			case p.Style == "form":
				query = append(query, name+"="+strings.Join(esc, ","))
			case p.Style == "spaceDelimited":
				query = append(query, name+"="+strings.Join(esc, "%20"))
			case p.Style == "pipeDelimited":
				query = append(query, name+"="+strings.Join(esc, "|"))
			default:
				return "", nil, fmt.Errorf("query parameter %s uses style %q, which this CLI cannot send", p.Name, p.Style)
			}
		case "header":
			if !validToken(p.Name) {
				return "", nil, fmt.Errorf("header parameter %q has an invalid name", p.Name)
			}
			headers.Set(p.Name, strings.Join(vals, ","))
		case "cookie":
			if p.Array {
				return "", nil, fmt.Errorf("cookie parameter %s is an array, which this CLI cannot send", p.Name)
			}
			if !validToken(p.Name) || !validCookieValue(vals[0]) {
				return "", nil, fmt.Errorf("--%s cannot be sent as a cookie: use letters, digits and punctuation other than space, quote, comma, semicolon and backslash", p.Name)
			}
			cookies = append(cookies, p.Name+"="+vals[0])
		default:
			return "", nil, fmt.Errorf("parameter %s is in %q, which this CLI cannot send", p.Name, p.In)
		}
	}
	if len(cookies) > 0 {
		headers.Set("Cookie", strings.Join(cookies, "; "))
	}
	target += path
	if len(query) > 0 {
		target += "?" + strings.Join(query, "&")
	}
	return target, headers, nil
}

// call maps flags onto one operation, sends it and prints the response.
func call(base *url.URL, a *api, o *operation, args []string, token string, stdin io.Reader, stdout, stderr io.Writer) int {
	values, body, err := parseFlags(o, args, stdin)
	if err != nil {
		return fail(stderr, exitUsage, err.Error())
	}
	target, headers, err := buildRequest(o, apiBase(base, a), values)
	if err != nil {
		return fail(stderr, exitUsage, err.Error())
	}
	req, err := http.NewRequest(o.Method, target, bytes.NewReader(body))
	if err != nil {
		return fail(stderr, exitError, err.Error())
	}
	req.Header = headers
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent())
	if body != nil {
		req.Header.Set("Content-Type", o.Body.MediaType)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 60 * time.Second, CheckRedirect: func(r *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("too many redirects")
		}
		if token != "" && !sameOrigin(r.URL, via[0].URL) {
			return fmt.Errorf("refusing to follow a redirect to another origin (%s) with MAILGLO_TOKEN", origin(r.URL))
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
		out = escapeJSON(pretty.Bytes())
	} else {
		out = []byte(cleanBody(string(out)))
	}
	code := exitCode(resp.StatusCode)
	w := stdout
	if resp.StatusCode >= 400 {
		w = stderr
	}
	if write(w, stderr, append(out, '\n')) != exitOK && code == exitOK {
		code = exitError
	}

	// A partial list is never presented as complete.
	if resp.Header.Get("X-Result-Complete") == "false" {
		warn(stderr, "incomplete result: repeat with --cursor "+resp.Header.Get("X-Next-Cursor")+" for the next page")
	}
	if n := resp.Header.Get("X-Changed-Since-Snapshot"); n != "" && n != "0" {
		warn(stderr, n+" item(s) changed since the first page; list again to see them")
	}
	if resp.Header.Get("Idempotent-Replay-Redacted") == "true" {
		warn(stderr, "replayed result: issued secrets are not shown again")
	}
	return code
}

func hasControl(s string) bool { return strings.ContainsFunc(s, isControl) }

// validToken reports whether s is an HTTP token (RFC 9110), as header and
// cookie names must be.
func validToken(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range []byte(s) {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0) {
			return false
		}
	}
	return true
}

// validCookieValue allows the cookie-octet characters of RFC 6265.
func validCookieValue(s string) bool {
	for _, c := range []byte(s) {
		if c < 0x21 || c > 0x7e || c == '"' || c == ',' || c == ';' || c == '\\' {
			return false
		}
	}
	return true
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
