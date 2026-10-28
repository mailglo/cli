package main

// End-to-end tests: each test runs the CLI as a separate process (this test
// binary re-executed as main) against an httptest server that serves a
// fixture OpenAPI description and records the API requests it receives.

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const runAsCLI = "MAILGLO_TEST_RUN_AS_CLI"

func TestMain(m *testing.M) {
	if os.Getenv(runAsCLI) == "1" {
		main()
		return
	}
	os.Exit(m.Run())
}

const testToken = "mgp_test-secret-token-value"

// unroutable is a TEST-NET-1 address: a CLI that tried to reach it would
// time out instead of failing fast, so any test using it also proves that
// no connection was attempted.
const unroutable = "http://192.0.2.1"

type recorded struct {
	Method, Path, RawQuery, Auth, ContentType, IdempotencyKey, Cookie string
	Body                                                              []byte
}

type fakeServer struct {
	*httptest.Server
	mu         sync.Mutex
	spec       []byte
	etag       string
	lastMod    string
	specStatus int // non-zero: answer the description request with this status
	// specRedirect, when set, redirects /api/openapi.json there; the
	// description is also served at any other path ending in openapi.json.
	specRedirect string
	specGets     []http.Header
	calls        []recorded
	respond      func(w http.ResponseWriter, r *http.Request)
}

// newFakeServer starts a server; configure runs before it accepts requests.
func newFakeServer(t *testing.T, tls bool, configure ...func(*fakeServer)) *fakeServer {
	t.Helper()
	spec, err := os.ReadFile(filepath.Join("testdata", "openapi.json"))
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeServer{spec: spec, etag: `"v1"`}
	for _, c := range configure {
		c(f)
	}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "openapi.json") {
			f.specGets = append(f.specGets, r.Header.Clone())
			if f.specRedirect != "" && r.URL.Path == "/api/openapi.json" {
				http.Redirect(w, r, f.specRedirect, http.StatusFound)
				return
			}
			if f.specStatus != 0 {
				w.WriteHeader(f.specStatus)
				return
			}
			if f.etag != "" {
				w.Header().Set("ETag", f.etag)
				if r.Header.Get("If-None-Match") == f.etag {
					w.WriteHeader(http.StatusNotModified)
					return
				}
			}
			if f.lastMod != "" {
				w.Header().Set("Last-Modified", f.lastMod)
				if f.etag == "" && r.Header.Get("If-Modified-Since") == f.lastMod {
					w.WriteHeader(http.StatusNotModified)
					return
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(completeSpec(f.spec))
			return
		}
		b, _ := io.ReadAll(r.Body)
		f.calls = append(f.calls, recorded{Method: r.Method, Path: r.URL.EscapedPath(), RawQuery: r.URL.RawQuery,
			Auth: r.Header.Get("Authorization"), ContentType: r.Header.Get("Content-Type"),
			IdempotencyKey: r.Header.Get("Idempotency-Key"), Cookie: r.Header.Get("Cookie"), Body: b})
		if f.respond != nil {
			f.respond(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	if tls {
		f.Server = httptest.NewTLSServer(h)
	} else {
		f.Server = httptest.NewServer(h)
	}
	t.Cleanup(f.Close)
	return f
}

// completeSpec fills in what OpenAPI requires but the tests' short
// descriptions leave out: info, a response for each operation and a schema
// for each parameter. Anything
// that is not a JSON object with paths is served as it is.
func completeSpec(raw []byte) []byte {
	var doc map[string]any
	if json.Unmarshal(raw, &doc) != nil {
		return raw
	}
	paths, ok := doc["paths"].(map[string]any)
	if !ok {
		return raw
	}
	if _, ok := doc["info"]; !ok {
		doc["info"] = map[string]any{"title": "Test API", "version": "1"}
	}
	// A parameter without schema or content gets a string schema.
	fill := func(list any) {
		params, _ := list.([]any)
		for _, p := range params {
			if p, ok := p.(map[string]any); ok && p["$ref"] == nil && p["schema"] == nil && p["content"] == nil {
				p["schema"] = map[string]any{"type": "string"}
			}
		}
	}
	if comps, ok := doc["components"].(map[string]any); ok {
		if ps, ok := comps["parameters"].(map[string]any); ok {
			for _, p := range ps {
				fill([]any{p})
			}
		}
	}
	for _, item := range paths {
		item, _ := item.(map[string]any)
		fill(item["parameters"])
		for _, m := range []string{"get", "put", "post", "delete", "options", "head", "patch", "trace"} {
			if op, ok := item[m].(map[string]any); ok {
				fill(op["parameters"])
				if _, ok := op["responses"]; !ok {
					op["responses"] = map[string]any{"200": map[string]any{"description": "OK"}}
				}
			}
		}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if enc.Encode(doc) != nil {
		return raw
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}

func (f *fakeServer) lastCall(t *testing.T) recorded {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		t.Fatal("the server received no API request")
	}
	return f.calls[len(f.calls)-1]
}

func (f *fakeServer) set(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

type result struct {
	Stdout, Stderr string
	Code           int
}

// env is one user's environment: a private cache directory and variables.
type env struct {
	cache  string
	vars   map[string]string
	stdin  string
	stdout *os.File // when set, the CLI writes its standard output here
	stderr *os.File // when set, the CLI writes its standard error here
}

func newEnv(t *testing.T, server string) *env {
	return &env{cache: t.TempDir(), vars: map[string]string{"MAILGLO_URL": server}}
}

func (e *env) run(t *testing.T, args ...string) result {
	t.Helper()
	return e.runBinary(t, os.Args[0], args...)
}

func (e *env) runBinary(t *testing.T, bin string, args ...string) result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "MAILGLO_") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	// os.UserCacheDir reads XDG_CACHE_HOME (Linux and BSD), HOME (macOS)
	// and LocalAppData (Windows).
	cmd.Env = append(cmd.Env, runAsCLI+"=1", "XDG_CACHE_HOME="+e.cache, "HOME="+e.cache, "LocalAppData="+e.cache)
	for k, v := range e.vars {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stdin = strings.NewReader(e.stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if e.stdout != nil {
		cmd.Stdout = e.stdout
	}
	if e.stderr != nil {
		cmd.Stderr = e.stderr
	}
	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("mailglo %v did not finish in time", args)
	}
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	r := result{Stdout: stdout.String(), Stderr: stderr.String(), Code: code}
	if strings.Contains(r.Stdout+r.Stderr, testToken) {
		t.Fatalf("the token appeared in the output:\n%s\n%s", r.Stdout, r.Stderr)
	}
	return r
}

func want(t *testing.T, r result, code int) {
	t.Helper()
	if r.Code != code {
		t.Fatalf("exit code %d, want %d\nstdout: %s\nstderr: %s", r.Code, code, r.Stdout, r.Stderr)
	}
}

func decode[T any](t *testing.T, s string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, s)
	}
	return v
}

func TestOperationsListsEveryOperation(t *testing.T) {
	srv := newFakeServer(t, false)
	r := newEnv(t, srv.URL).run(t, "operations")
	want(t, r, 0)
	list := decode[[]map[string]string](t, r.Stdout)
	var got []string
	for _, o := range list {
		got = append(got, o["method"]+" "+o["operation"]+" "+o["path"])
	}
	wantList := []string{
		"POST add-note /domains/{domainId}/notes",
		"POST create-domain /domains",
		"DELETE delete-domain /domains/{domainId}",
		"GET get-domain /domains/{domainId}",
		"GET list-domains /domains",
	}
	if strings.Join(got, "\n") != strings.Join(wantList, "\n") {
		t.Fatalf("operations:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(wantList, "\n"))
	}
	if list[4]["summary"] != "List domains." {
		t.Fatalf("summary %q", list[4]["summary"])
	}
}

func TestHelpShowsParametersAndBody(t *testing.T) {
	srv := newFakeServer(t, false)
	e := newEnv(t, srv.URL)

	r := e.run(t, "help", "create-domain")
	want(t, r, 0)
	info := decode[map[string]any](t, r.Stdout)
	if info["method"] != "POST" || info["path"] != "/domains" || info["summary"] != "Add a domain." {
		t.Fatalf("help: %v", info)
	}
	if fmt.Sprint(info["body_fields"]) != "[name policy]" || fmt.Sprint(info["body_required"]) != "[name]" {
		t.Fatalf("body fields from allOf and $ref: %v %v", info["body_fields"], info["body_required"])
	}
	params := info["parameters"].([]any)
	if len(params) != 1 || params[0].(map[string]any)["flag"] != "--Idempotency-Key" {
		t.Fatalf("parameters: %v", params)
	}

	// The operationId works as well as the kebab-case name, and a path-level
	// parameter given by $ref is listed as required.
	r = e.run(t, "help", "AddNote")
	want(t, r, 0)
	info = decode[map[string]any](t, r.Stdout)
	p := info["parameters"].([]any)[0].(map[string]any)
	if p["flag"] != "--domainId" || p["in"] != "path" || p["required"] != true || p["description"] != "Domain identifier." {
		t.Fatalf("path parameter: %v", p)
	}
	if fmt.Sprint(info["body_fields"]) != "[text]" {
		t.Fatalf("body from requestBodies $ref: %v", info["body_fields"])
	}

	want(t, e.run(t, "help", "no-such-operation"), 2)
	r = e.run(t, "help")
	want(t, r, 0)
	if !strings.Contains(r.Stdout, "usage:") {
		t.Fatalf("help without an operation: %q", r.Stdout)
	}
}

func TestRequestMapping(t *testing.T) {
	srv := newFakeServer(t, false)
	e := newEnv(t, srv.URL)
	e.vars["MAILGLO_TOKEN"] = testToken

	t.Run("path parameter is escaped", func(t *testing.T) {
		r := e.run(t, "get-domain", "--domainId", "a b?c#d")
		want(t, r, 0)
		c := srv.lastCall(t)
		if c.Method != "GET" || c.Path != "/api/v1/domains/a%20b%3Fc%23d" || c.Auth != "Bearer "+testToken {
			t.Fatalf("call: %+v", c)
		}
		if decode[map[string]bool](t, r.Stdout)["ok"] != true {
			t.Fatalf("stdout: %s", r.Stdout)
		}
	})
	t.Run("query parameters", func(t *testing.T) {
		want(t, e.run(t, "list-domains", "--status", "active", "--cursor=c 1"), 0)
		if c := srv.lastCall(t); c.Path != "/api/v1/domains" || c.RawQuery != "cursor=c+1&status=active" {
			t.Fatalf("call: %+v", c)
		}
	})
	t.Run("header parameter and inline body", func(t *testing.T) {
		want(t, e.run(t, "create-domain", "--Idempotency-Key", "k1", "--body", `{"name":"example.com"}`), 0)
		c := srv.lastCall(t)
		if c.Method != "POST" || c.IdempotencyKey != "k1" || c.ContentType != "application/json" || string(c.Body) != `{"name":"example.com"}` {
			t.Fatalf("call: %+v body %s", c, c.Body)
		}
	})
	t.Run("body from file", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "note.json")
		if err := os.WriteFile(f, []byte(`{"text":"from a file"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		want(t, e.run(t, "add-note", "--domainId", "d1", "--body", "@"+f), 0)
		if c := srv.lastCall(t); c.Path != "/api/v1/domains/d1/notes" || string(c.Body) != `{"text":"from a file"}` {
			t.Fatalf("call: %+v body %s", c, c.Body)
		}
	})
	t.Run("body from stdin", func(t *testing.T) {
		e.stdin = `{"text":"from stdin"}`
		defer func() { e.stdin = "" }()
		want(t, e.run(t, "add-note", "--domainId", "d2", "--body", "-"), 0)
		if c := srv.lastCall(t); string(c.Body) != `{"text":"from stdin"}` {
			t.Fatalf("body %s", c.Body)
		}
	})
	t.Run("no body sends no content type", func(t *testing.T) {
		want(t, e.run(t, "delete-domain", "--domainId", "d3"), 0)
		if c := srv.lastCall(t); c.Method != "DELETE" || c.ContentType != "" || len(c.Body) != 0 {
			t.Fatalf("call: %+v", c)
		}
	})
	t.Run("usage errors send nothing", func(t *testing.T) {
		srv.set(func() { srv.calls = nil })
		for _, args := range [][]string{
			{"get-domain"}, // missing path parameter
			{"get-domain", "--domainId", "d", "--nope", "x"}, // unknown flag
			{"get-domain", "--domainId"},                     // flag without a value
			{"get-domain", "domainId", "d"},                  // not a flag
			{"create-domain", "--body", "{not json"},         // invalid body
			{"add-note", "--domainId", "d", "--body", "@/no/such/file"},
			{"no-such-operation"},
		} {
			r := e.run(t, args...)
			if r.Code != 2 {
				t.Fatalf("%v: exit %d, want 2; stderr %s", args, r.Code, r.Stderr)
			}
		}
		srv.set(func() {
			if len(srv.calls) != 0 {
				t.Fatalf("usage errors reached the API: %+v", srv.calls)
			}
		})
	})
	t.Run("the description request carries no token", func(t *testing.T) {
		srv.set(func() {
			for _, h := range srv.specGets {
				if h.Get("Authorization") != "" {
					t.Fatal("the API description was requested with the token")
				}
			}
		})
	})
}

func TestNoArgumentsIsUsage(t *testing.T) {
	r := newEnv(t, unroutable).run(t)
	want(t, r, 2)
	if !strings.Contains(r.Stderr, "usage:") {
		t.Fatalf("stderr: %s", r.Stderr)
	}
}

func TestExitCodes(t *testing.T) {
	srv := newFakeServer(t, false)
	e := newEnv(t, srv.URL)
	cases := map[int]int{
		200: 0, 201: 0, 204: 0,
		400: 1, 422: 1,
		401: 3, 403: 3,
		404: 4, 410: 4,
		402: 5, 409: 5,
		429: 6, 500: 6, 502: 6, 503: 6,
	}
	for status, code := range cases {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			srv.set(func() {
				srv.respond = func(w http.ResponseWriter, r *http.Request) {
					if status == 204 {
						w.WriteHeader(status)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(status)
					_, _ = fmt.Fprintf(w, `{"error":"status %d"}`, status)
				}
			})
			r := e.run(t, "get-domain", "--domainId", "d")
			want(t, r, code)
			out, other := r.Stdout, r.Stderr
			if status >= 400 {
				out, other = r.Stderr, r.Stdout
			}
			wantOut := fmt.Sprintf(`"error": "status %d"`, status)
			if status == 204 {
				wantOut = `"status": 204`
			}
			if !strings.Contains(out, wantOut) || strings.Contains(other, "status") {
				t.Fatalf("stdout %q stderr %q", r.Stdout, r.Stderr)
			}
		})
	}
	t.Run("server unreachable", func(t *testing.T) {
		// The description is cached by now; the API call itself fails.
		srv.Close()
		r := e.run(t, "get-domain", "--domainId", "d")
		want(t, r, 6)
		if !strings.Contains(r.Stderr, "using the copy cached") || !strings.Contains(r.Stderr, "request failed") {
			t.Fatalf("stderr: %s", r.Stderr)
		}
	})
}

func TestPartialResultWarnings(t *testing.T) {
	srv := newFakeServer(t, false, func(f *fakeServer) {
		f.respond = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Result-Complete", "false")
			w.Header().Set("X-Next-Cursor", "next-1")
			w.Header().Set("X-Changed-Since-Snapshot", "2")
			w.Header().Set("Idempotent-Replay-Redacted", "true")
			_, _ = w.Write([]byte(`[]`))
		}
	})
	r := newEnv(t, srv.URL).run(t, "list-domains")
	want(t, r, 0)
	for _, s := range []string{"--cursor next-1", "2 item(s) changed", "issued secrets are not shown again"} {
		if !strings.Contains(r.Stderr, s) {
			t.Fatalf("stderr lacks %q: %s", s, r.Stderr)
		}
	}
}

func TestDescriptionCache(t *testing.T) {
	t.Run("revalidates with ETag", func(t *testing.T) {
		srv := newFakeServer(t, false)
		e := newEnv(t, srv.URL)
		want(t, e.run(t, "operations"), 0)
		r := e.run(t, "operations")
		want(t, r, 0)
		srv.set(func() {
			if len(srv.specGets) != 2 || srv.specGets[0].Get("If-None-Match") != "" || srv.specGets[1].Get("If-None-Match") != `"v1"` {
				t.Fatalf("description requests: %v", srv.specGets)
			}
		})
		if len(decode[[]map[string]string](t, r.Stdout)) != 5 || r.Stderr != "" {
			t.Fatalf("after 304: %s %s", r.Stdout, r.Stderr)
		}

		// A new server release adds an operation; the CLI picks it up.
		srv.set(func() {
			srv.etag = `"v2"`
			var doc map[string]any
			if err := json.Unmarshal(srv.spec, &doc); err != nil {
				t.Fatal(err)
			}
			item := doc["paths"].(map[string]any)["/domains/{domainId}"].(map[string]any)
			item["put"] = map[string]any{"operationId": "RenameDomain", "summary": "Rename."}
			srv.spec, _ = json.Marshal(doc)
		})
		r = e.run(t, "operations")
		want(t, r, 0)
		if !strings.Contains(r.Stdout, `"rename-domain"`) {
			t.Fatalf("new operation missing: %s", r.Stdout)
		}
		srv.set(func() { srv.specStatus = http.StatusServiceUnavailable })
		r = e.run(t, "operations")
		want(t, r, 0)
		if !strings.Contains(r.Stdout, `"rename-domain"`) {
			t.Fatalf("the cache holds the old description: %s", r.Stdout)
		}
	})

	t.Run("revalidates with Last-Modified", func(t *testing.T) {
		srv := newFakeServer(t, false, func(f *fakeServer) { f.etag, f.lastMod = "", "Mon, 26 Oct 2026 10:00:00 GMT" })
		e := newEnv(t, srv.URL)
		want(t, e.run(t, "operations"), 0)
		want(t, e.run(t, "operations"), 0)
		srv.set(func() {
			if len(srv.specGets) != 2 || srv.specGets[1].Get("If-Modified-Since") != srv.lastMod || srv.specGets[1].Get("If-None-Match") != "" {
				t.Fatalf("description requests: %v", srv.specGets)
			}
		})
	})

	t.Run("falls back to the cache with a warning", func(t *testing.T) {
		srv := newFakeServer(t, false)
		e := newEnv(t, srv.URL)
		want(t, e.run(t, "operations"), 0)

		srv.set(func() { srv.specStatus = http.StatusInternalServerError })
		r := e.run(t, "help", "get-domain")
		want(t, r, 0)
		if !strings.Contains(r.Stderr, "warning: cannot fetch the API description") || !strings.Contains(r.Stderr, "HTTP 500") {
			t.Fatalf("stderr: %s", r.Stderr)
		}

		srv.Close()
		r = e.run(t, "operations")
		want(t, r, 0)
		if !strings.Contains(r.Stderr, "using the copy cached") || len(decode[[]map[string]string](t, r.Stdout)) != 5 {
			t.Fatalf("stdout %s stderr %s", r.Stdout, r.Stderr)
		}
	})

	t.Run("a server that is not serving a description", func(t *testing.T) {
		srv := newFakeServer(t, false, func(f *fakeServer) { f.spec = []byte(`<html>maintenance</html>`) })
		r := newEnv(t, srv.URL).run(t, "operations")
		want(t, r, 6)
	})

	t.Run("no cache and no server exits 6", func(t *testing.T) {
		srv := newFakeServer(t, false, func(f *fakeServer) { f.specStatus = http.StatusBadGateway })
		r := newEnv(t, srv.URL).run(t, "operations")
		want(t, r, 6)
		if !strings.Contains(r.Stderr, "retry later") {
			t.Fatalf("stderr: %s", r.Stderr)
		}
	})

	t.Run("each server has its own cache", func(t *testing.T) {
		a := newFakeServer(t, false)
		b := newFakeServer(t, false, func(f *fakeServer) {
			f.spec = []byte(`{"openapi":"3.0.3","paths":{"/ping":{"get":{"operationId":"Ping"}}}}`)
		})
		e := newEnv(t, a.URL)
		want(t, e.run(t, "operations"), 0)
		r := e.run(t, "--server", b.URL, "operations")
		want(t, r, 0)
		if !strings.Contains(r.Stdout, `"ping"`) || strings.Contains(r.Stdout, "list-domains") {
			t.Fatalf("server b: %s", r.Stdout)
		}
		entries, err := os.ReadDir(filepath.Dir(cacheFileIn(t, e.cache, a.URL)))
		if err != nil || len(entries) != 2 {
			t.Fatalf("cache entries: %v %v", entries, err)
		}
	})
}

// cacheFileIn finds the cache file the CLI would use for server under a
// cache root set by env.
func cacheFileIn(t *testing.T, root, server string) string {
	t.Helper()
	var found string
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasPrefix(d.Name(), "spec-") {
			b, _ := os.ReadFile(p)
			if strings.Contains(string(b), `"server":"`+server+`"`) {
				found = p
			}
		}
		return nil
	})
	if found == "" {
		t.Fatalf("no cache file for %s under %s", server, root)
	}
	return found
}

func TestServerFlagOverridesEnvironment(t *testing.T) {
	srv := newFakeServer(t, false)
	e := newEnv(t, unroutable)
	want(t, e.run(t, "--server", srv.URL, "get-domain", "--domainId", "d"), 0)
	want(t, e.run(t, "--server="+srv.URL+"/", "get-domain", "--domainId", "e"), 0)
	if c := srv.lastCall(t); c.Path != "/api/v1/domains/e" {
		t.Fatalf("call: %+v", c)
	}
	for _, bad := range []string{"ftp://example.com", "https://user:pass@example.com", "not a url", "https://example.com?x=1"} {
		r := e.run(t, "--server", bad, "operations")
		want(t, r, 2)
		if strings.Contains(r.Stderr, "pass") {
			t.Fatalf("credentials echoed: %s", r.Stderr)
		}
	}
	want(t, e.run(t, "--server"), 2)
}

func TestTokenNeedsHTTPS(t *testing.T) {
	t.Run("refused for plain http to another host", func(t *testing.T) {
		for _, server := range []string{unroutable, "http://mailglo.example", "http://[2001:db8::1]:8080"} {
			e := newEnv(t, server)
			e.vars["MAILGLO_TOKEN"] = testToken
			r := e.run(t, "get-domain", "--domainId", "d")
			want(t, r, 2)
			if !strings.Contains(r.Stderr, "refusing to send MAILGLO_TOKEN") {
				t.Fatalf("%s: stderr %s", server, r.Stderr)
			}
		}
	})
	t.Run("allowed for plain http to this machine", func(t *testing.T) {
		srv := newFakeServer(t, false)
		port := srv.URL[strings.LastIndex(srv.URL, ":"):]
		for _, server := range []string{srv.URL, "http://localhost" + port} {
			e := newEnv(t, server)
			e.vars["MAILGLO_TOKEN"] = testToken
			want(t, e.run(t, "get-domain", "--domainId", "d"), 0)
			if c := srv.lastCall(t); c.Auth != "Bearer "+testToken {
				t.Fatalf("%s: auth %q", server, c.Auth)
			}
		}
	})
	t.Run("plain http without a token is allowed", func(t *testing.T) {
		srv := newFakeServer(t, false)
		want(t, newEnv(t, srv.URL).run(t, "get-domain", "--domainId", "d"), 0)
		if c := srv.lastCall(t); c.Auth != "" {
			t.Fatalf("auth %q", c.Auth)
		}
	})
	t.Run("not sent on a redirect to plain http", func(t *testing.T) {
		srv := newFakeServer(t, false, func(f *fakeServer) {
			f.respond = func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, unroutable+"/api/v1/domains/d", http.StatusFound)
			}
		})
		e := newEnv(t, srv.URL)
		e.vars["MAILGLO_TOKEN"] = testToken
		r := e.run(t, "get-domain", "--domainId", "d")
		want(t, r, 6)
		if !strings.Contains(r.Stderr, "refusing to follow a redirect") {
			t.Fatalf("stderr: %s", r.Stderr)
		}
	})
	t.Run("sent over https", func(t *testing.T) {
		if runtime.GOOS != "linux" {
			t.Skip("trusting the test certificate through SSL_CERT_FILE works on Linux only")
		}
		srv := newFakeServer(t, true)
		certFile := filepath.Join(t.TempDir(), "cert.pem")
		pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
		if err := os.WriteFile(certFile, pemBytes, 0o600); err != nil {
			t.Fatal(err)
		}
		e := newEnv(t, srv.URL)
		e.vars["MAILGLO_TOKEN"] = testToken
		e.vars["SSL_CERT_FILE"] = certFile
		want(t, e.run(t, "get-domain", "--domainId", "d"), 0)
		if c := srv.lastCall(t); c.Auth != "Bearer "+testToken {
			t.Fatalf("auth %q", c.Auth)
		}
	})
}

func TestVersion(t *testing.T) {
	t.Run("needs no server", func(t *testing.T) {
		for _, arg := range []string{"version", "--version"} {
			r := newEnv(t, unroutable).run(t, arg)
			want(t, r, 0)
			v := decode[map[string]string](t, r.Stdout)
			if v["version"] == "" || !strings.HasPrefix(v["go"], "go") {
				t.Fatalf("version: %v", v)
			}
		}
	})
	t.Run("release build stamps the version", func(t *testing.T) {
		if testing.Short() {
			t.Skip("builds the binary")
		}
		bin := filepath.Join(t.TempDir(), "mailglo")
		if runtime.GOOS == "windows" {
			bin += ".exe"
		}
		build := exec.Command("go", "build", "-o", bin,
			"-ldflags", "-X main.version=1.2.3 -X main.commit=abc123 -X main.date=2026-10-27T00:00:00Z", ".")
		if out, err := build.CombinedOutput(); err != nil {
			t.Fatalf("go build: %v\n%s", err, out)
		}
		r := newEnv(t, unroutable).runBinary(t, bin, "version")
		want(t, r, 0)
		v := decode[map[string]string](t, r.Stdout)
		if v["version"] != "1.2.3" || v["commit"] != "abc123" || v["date"] != "2026-10-27T00:00:00Z" {
			t.Fatalf("version: %v", v)
		}

		// The release binary sends its version to the server.
		var ua string
		srv := newFakeServer(t, false, func(f *fakeServer) {
			f.respond = func(w http.ResponseWriter, r *http.Request) { ua = r.UserAgent(); _, _ = w.Write([]byte(`{}`)) }
		})
		want(t, newEnv(t, srv.URL).runBinary(t, bin, "get-domain", "--domainId", "d"), 0)
		srv.set(func() {
			if ua != "mailglo-cli/1.2.3" {
				t.Fatalf("user agent %q", ua)
			}
		})
	})
}
