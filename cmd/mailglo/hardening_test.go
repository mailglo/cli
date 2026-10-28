package main

// End-to-end tests for hostile or unusual API descriptions, redirects,
// cache tampering and output failures.

import (
	"bytes"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"
)

// specServer serves spec instead of the fixture.
func specServer(t *testing.T, spec string, configure ...func(*fakeServer)) *fakeServer {
	t.Helper()
	if !json.Valid([]byte(spec)) {
		t.Fatalf("test spec is not JSON: %s", spec)
	}
	return newFakeServer(t, false, append([]func(*fakeServer){func(f *fakeServer) { f.spec = []byte(spec) }}, configure...)...)
}

// cacheDirOf is where the CLI keeps descriptions for e.
func cacheDirOf(e *env) string {
	if runtime.GOOS == "darwin" {
		return filepath.Join(e.cache, "Library", "Caches", "mailglo")
	}
	return filepath.Join(e.cache, "mailglo")
}

func trustCert(t *testing.T, e *env, srv *fakeServer) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("trusting the test certificate through SSL_CERT_FILE works on Linux only")
	}
	certFile := filepath.Join(t.TempDir(), "cert.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(certFile, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	e.vars["SSL_CERT_FILE"] = certFile
}

func (f *fakeServer) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeServer) specCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.specGets)
}

// Item 1: an authenticated redirect must stay on the same origin.
func TestTokenStaysOnOrigin(t *testing.T) {
	other := newFakeServer(t, false)
	srv := newFakeServer(t, false)
	port := srv.URL[strings.LastIndex(srv.URL, ":")+1:]
	var redirectTo string
	srv.set(func() {
		srv.respond = func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/domains/d" {
				http.Redirect(w, r, redirectTo, http.StatusFound)
				return
			}
			_, _ = w.Write([]byte(`{"followed":true}`))
		}
	})
	e := newEnv(t, srv.URL)
	e.vars["MAILGLO_TOKEN"] = testToken
	for _, target := range []string{
		"http://sub.localhost:" + port + "/api/v1/domains/x", // subdomain
		"http://localhost:" + port + "/api/v1/domains/x",     // other host name
		other.URL + "/api/v1/domains/x",                      // other port
		"https://127.0.0.1:" + port + "/api/v1/domains/x",    // other scheme
	} {
		srv.set(func() { redirectTo = target })
		r := e.run(t, "get-domain", "--domainId", "d")
		want(t, r, 6)
		if !strings.Contains(r.Stderr, "redirect to another origin") {
			t.Fatalf("%s: stderr %s", target, r.Stderr)
		}
	}
	if other.callCount() != 0 {
		t.Fatal("the token reached another port")
	}

	// A same-origin redirect keeps the token.
	srv.set(func() { redirectTo = "/api/v1/domains/moved" })
	r := e.run(t, "get-domain", "--domainId", "d")
	want(t, r, 0)
	if c := srv.lastCall(t); c.Path != "/api/v1/domains/moved" || c.Auth != "Bearer "+testToken {
		t.Fatalf("call: %+v", c)
	}

	// Without a token the request can still carry credentials (a cookie
	// or header parameter), so a cross-origin redirect is refused too.
	srv.set(func() { redirectTo = other.URL + "/api/v1/domains/x" })
	r = newEnv(t, srv.URL).run(t, "get-domain", "--domainId", "d")
	want(t, r, 6)
	if other.callCount() != 0 || !strings.Contains(r.Stderr, "redirect to another origin") {
		t.Fatalf("followed a cross-origin redirect without a token: %s", r.Stderr)
	}
}

// Item 2: description redirects stay on the server's origin.
func TestDescriptionRedirectsStayOnOrigin(t *testing.T) {
	poisoned := `{"openapi":"3.0.3","paths":{"/domains/{domainId}":{"delete":{"operationId":"GetDomain","parameters":[{"name":"domainId","in":"path","required":true}]}}}}`
	evil := specServer(t, poisoned)

	srv := newFakeServer(t, false, func(f *fakeServer) { f.specRedirect = evil.URL + "/api/openapi.json" })
	r := newEnv(t, srv.URL).run(t, "operations")
	want(t, r, 6)
	if !strings.Contains(r.Stderr, "redirect to another origin") || evil.specCount() != 0 {
		t.Fatalf("stderr %s; other server asked %d times", r.Stderr, evil.specCount())
	}

	same := newFakeServer(t, false, func(f *fakeServer) { f.specRedirect = "/v2/openapi.json" })
	r = newEnv(t, same.URL).run(t, "operations")
	want(t, r, 0)
	if !strings.Contains(r.Stdout, "list-domains") {
		t.Fatalf("same-origin redirect: %s", r.Stdout)
	}

	t.Run("https to http downgrade", func(t *testing.T) {
		tlsSrv := newFakeServer(t, true, func(f *fakeServer) { f.specRedirect = evil.URL + "/api/openapi.json" })
		e := newEnv(t, tlsSrv.URL)
		trustCert(t, e, tlsSrv)
		r := e.run(t, "operations")
		want(t, r, 6)
		if evil.specCount() != 0 {
			t.Fatal("followed a downgrade to http")
		}
	})
}

// Item 3: repeated and cyclic allOf references cost linear time, and a
// total budget stops descriptions that are still too large to expand.
func TestHostileSchemasAreCheap(t *testing.T) {
	schemas := map[string]any{"Self": map[string]any{"allOf": []any{}}}
	self := schemas["Self"].(map[string]any)
	for i := 0; i < 8; i++ {
		self["allOf"] = append(self["allOf"].([]any), map[string]any{"$ref": "#/components/schemas/Self"})
	}
	for i := 0; i < 30; i++ {
		var all []any
		for j := 0; j < 8; j++ {
			all = append(all, map[string]any{"$ref": fmt.Sprintf("#/components/schemas/S%d", i+1)})
		}
		all = append(all, map[string]any{"$ref": "#/components/schemas/Self"})
		schemas[fmt.Sprintf("S%d", i)] = map[string]any{"allOf": all}
	}
	schemas["S30"] = map[string]any{"properties": map[string]any{"deep": map[string]any{}}}
	spec := map[string]any{"openapi": "3.0.3", "components": map[string]any{"schemas": schemas},
		"paths": map[string]any{"/x": map[string]any{"post": map[string]any{"operationId": "Bomb",
			"requestBody": map[string]any{"content": map[string]any{"application/json": map[string]any{
				"schema": map[string]any{"$ref": "#/components/schemas/S0"}}}}}}}}
	b, _ := json.Marshal(spec)
	r := newEnv(t, specServer(t, string(b)).URL).run(t, "help", "bomb")
	want(t, r, 0)
	if !strings.Contains(r.Stdout, `"deep"`) {
		t.Fatalf("help: %s", r.Stdout)
	}

	// 3000 operations, each with a 400-schema chain: over the budget.
	chain := map[string]any{}
	for i := 0; i < 400; i++ {
		chain[fmt.Sprintf("C%d", i)] = map[string]any{"allOf": []any{map[string]any{"$ref": fmt.Sprintf("#/components/schemas/C%d", i+1)}}}
	}
	paths := map[string]any{}
	for i := 0; i < 3000; i++ {
		paths[fmt.Sprintf("/p%d", i)] = map[string]any{"post": map[string]any{"operationId": fmt.Sprintf("Op%d", i),
			"requestBody": map[string]any{"content": map[string]any{"application/json": map[string]any{
				"schema": map[string]any{"$ref": "#/components/schemas/C0"}}}}}}
	}
	b, _ = json.Marshal(map[string]any{"openapi": "3.0.3", "components": map[string]any{"schemas": chain}, "paths": paths})
	r = newEnv(t, specServer(t, string(b)).URL).run(t, "operations")
	want(t, r, 6)
	if !strings.Contains(r.Stderr, "too complex") {
		t.Fatalf("stderr: %s", r.Stderr)
	}
}

// Item 4: the cache is trusted only when this user alone controls it.
func TestCacheMustBePrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permissions")
	}
	poison := func(t *testing.T, path, server string) {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"server": server, "fetched_at": "2026-01-01T00:00:00Z",
			"spec": json.RawMessage(`{"openapi":"3.0.3","paths":{"/x":{"delete":{"operationId":"Poisoned"}}}}`)})
		if err := os.WriteFile(path, b, 0o666); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o666); err != nil {
			t.Fatal(err)
		}
	}
	populate := func(t *testing.T) (*fakeServer, *env, string) {
		srv := newFakeServer(t, false)
		e := newEnv(t, srv.URL)
		want(t, e.run(t, "operations"), 0)
		return srv, e, cacheFileIn(t, e.cache, srv.URL)
	}

	t.Run("an open directory of ours is tightened", func(t *testing.T) {
		srv := newFakeServer(t, false)
		e := newEnv(t, srv.URL)
		dir := cacheDirOf(e)
		if err := os.MkdirAll(dir, 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o777); err != nil {
			t.Fatal(err)
		}
		want(t, e.run(t, "operations"), 0)
		fi, err := os.Stat(dir)
		if err != nil || fi.Mode().Perm() != 0o700 {
			t.Fatalf("mode %v %v", fi.Mode(), err)
		}
	})
	t.Run("a file others can write is ignored", func(t *testing.T) {
		srv, e, path := populate(t)
		poison(t, path, srv.URL)
		srv.set(func() { srv.specStatus = http.StatusInternalServerError })
		r := e.run(t, "operations")
		want(t, r, 6)
		if !strings.Contains(r.Stderr, "writable by other users") || strings.Contains(r.Stdout, "poisoned") {
			t.Fatalf("stdout %s stderr %s", r.Stdout, r.Stderr)
		}
	})
	t.Run("a symlinked file is ignored", func(t *testing.T) {
		srv, e, path := populate(t)
		elsewhere := filepath.Join(t.TempDir(), "spec.json")
		if err := os.Rename(path, elsewhere); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(elsewhere, path); err != nil {
			t.Fatal(err)
		}
		srv.set(func() { srv.specStatus = http.StatusInternalServerError })
		r := e.run(t, "operations")
		want(t, r, 6)
		if !strings.Contains(r.Stderr, "not a regular file") {
			t.Fatalf("stderr %s", r.Stderr)
		}
	})
	t.Run("a symlinked directory is not used", func(t *testing.T) {
		srv, e, path := populate(t)
		target := t.TempDir()
		if err := os.Rename(path, filepath.Join(target, filepath.Base(path))); err != nil {
			t.Fatal(err)
		}
		dir := cacheDirOf(e)
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, dir); err != nil {
			t.Fatal(err)
		}
		r := e.run(t, "operations")
		want(t, r, 0)
		if !strings.Contains(r.Stderr, "not using the cache") {
			t.Fatalf("stderr %s", r.Stderr)
		}
		srv.set(func() { srv.specStatus = http.StatusInternalServerError })
		want(t, e.run(t, "operations"), 6)
		entries, _ := os.ReadDir(target)
		if len(entries) != 1 {
			t.Fatalf("wrote into the symlinked directory: %v", entries)
		}
	})
}

// Item 5: path values cannot change which route the server picks.
func TestPathValuesCannotChangeRoute(t *testing.T) {
	srv := newFakeServer(t, false)
	e := newEnv(t, srv.URL)
	for _, v := range []string{"..", ".", "", "a/b", `a\b`, "x\ny"} {
		r := e.run(t, "add-note", "--domainId", v, "--body", `{"text":"t"}`)
		if r.Code != 2 {
			t.Fatalf("%q: exit %d, stderr %s", v, r.Code, r.Stderr)
		}
	}
	if srv.callCount() != 0 {
		t.Fatal("a rejected value reached the server")
	}
	want(t, e.run(t, "get-domain", "--domainId", "%2e%2e"), 0)
	if c := srv.lastCall(t); c.Path != "/api/v1/domains/%252e%252e" {
		t.Fatalf("path %s", c.Path)
	}
}

func hasRawControl(s string) bool {
	return !utf8.ValidString(s) || strings.ContainsFunc(s, func(r rune) bool {
		return (r < 0x20 && r != '\n' && r != '\t') || r == 0x7f || (r >= 0x80 && r <= 0x9f)
	})
}

// Item 6: control characters from the description or the server never reach
// the terminal.
func TestControlCharactersAreEscaped(t *testing.T) {
	osc := "\x1b]52;c;aGk=\x07"
	spec, _ := json.Marshal(map[string]any{"openapi": "3.0.3", "paths": map[string]any{
		"/x": map[string]any{"get": map[string]any{
			"operationId": "Evil" + osc + "Op", "summary": "sum\u009b31m\x1b[2J",
			"parameters": []any{map[string]any{"name": "p" + osc, "in": "query", "required": true, "description": "d\u0085" + osc}},
		}},
		"/y": map[string]any{"get": map[string]any{"operationId": "Reply"}},
	}})
	srv := specServer(t, string(spec), func(f *fakeServer) {
		f.respond = func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("{\"s\":\"c1 \u009b del \x7f\",\"bad\":\"\xff\"}"))
		}
	})
	e := newEnv(t, srv.URL)
	r := e.run(t, "operations")
	want(t, r, 0)
	if hasRawControl(r.Stdout + r.Stderr) {
		t.Fatalf("raw control characters: %q", r.Stdout)
	}
	var name string
	for _, o := range decode[[]map[string]string](t, r.Stdout) {
		if strings.HasPrefix(o["operation"], "evil") {
			name = o["operation"]
			if o["operation_id"] != "Evil"+osc+"Op" || o["summary"] != "sum\u009b31m\x1b[2J" {
				t.Fatalf("escaped JSON must decode to the original text: %q", o)
			}
		}
	}
	if name == "" || hasRawControl(name) || strings.ContainsAny(name, "\x1b\x07;]=") {
		t.Fatalf("command name %q", name)
	}
	for _, args := range [][]string{{"help", name}, {name}, {"Evil" + osc}} {
		r = e.run(t, args...)
		if hasRawControl(r.Stdout + r.Stderr) {
			t.Fatalf("%q: raw control characters: %q %q", args, r.Stdout, r.Stderr)
		}
	}
	if r := e.run(t, name); r.Code != 2 || !strings.Contains(r.Stderr, `\x1b]52`) {
		t.Fatalf("missing-parameter diagnostic: %q", r.Stderr)
	}
	r = e.run(t, "reply")
	want(t, r, 0)
	if hasRawControl(r.Stdout) || decode[map[string]string](t, r.Stdout)["s"] != "c1 \u009b del \x7f" {
		t.Fatalf("JSON response: %q", r.Stdout)
	}
}

func TestNonJSONResponseIsEscaped(t *testing.T) {
	srv := newFakeServer(t, false, func(f *fakeServer) {
		f.respond = func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("line one\nnot json \x1b]52;c;aGk=\x07 \u009b"))
		}
	})
	r := newEnv(t, srv.URL).run(t, "get-domain", "--domainId", "d")
	want(t, r, 0)
	if hasRawControl(r.Stdout) || !strings.Contains(r.Stdout, "line one\nnot json \\x1b]52") {
		t.Fatalf("stdout %q", r.Stdout)
	}
}

// Item 7: the base path comes from "servers", on the selected origin.
func TestServersBasePath(t *testing.T) {
	paths := `"paths":{"/things":{"get":{"operationId":"ListThings"}}}`
	cases := []struct{ servers, want string }{
		{``, "/api/v1/things"},
		{`"servers":[{"url":"/api/v2"}],`, "/api/v2/things"},
		{`"servers":[{"url":"v9/"}],`, "/api/v9/things"},
		{`"servers":[{"url":"https://elsewhere.example/base"}],`, "/base/things"},
		{`"servers":[{"url":"{root}/x","variables":{"root":{"default":"/api/v3"}}}],`, "/api/v3/x/things"},
	}
	for _, c := range cases {
		srv := specServer(t, `{"openapi":"3.0.3",`+c.servers+paths+`}`)
		e := newEnv(t, srv.URL)
		e.vars["MAILGLO_TOKEN"] = testToken
		want(t, e.run(t, "list-things"), 0)
		if got := srv.lastCall(t); got.Path != c.want || got.Auth != "Bearer "+testToken {
			t.Fatalf("servers %s: path %s auth %q, want %s", c.servers, got.Path, got.Auth, c.want)
		}
	}
}

// Items 8 and 9: command names are unique and deterministic, including for
// operations without an operationId.
func TestCommandNames(t *testing.T) {
	spec := `{"openapi":"3.0.3","paths":{
		"/thing":{"get":{"operationId":"GetThing"},"delete":{"operationId":"getthing"}},
		"/items":{"get":{"operationId":"ListItems"},"post":{"operationId":"list-items"}},
		"/health":{"get":{"summary":"No id."}},
		"/domains/{id}/notes":{"post":{"parameters":[{"name":"id","in":"path","required":true}]}}}}`
	srv := specServer(t, spec)
	e := newEnv(t, srv.URL)
	first := e.run(t, "operations")
	want(t, first, 0)
	for i := 0; i < 5; i++ {
		if r := e.run(t, "operations"); r.Stdout != first.Stdout {
			t.Fatal("operations output changes between runs")
		}
	}
	var names []string
	for _, o := range decode[[]map[string]string](t, first.Stdout) {
		names = append(names, o["operation"]+"="+o["method"])
	}
	wantNames := "get-health=GET get-thing=GET getthing=DELETE list-items-get=GET list-items-post=POST post-domains-id-notes=POST"
	if strings.Join(names, " ") != wantNames {
		t.Fatalf("names: %v", names)
	}
	for _, c := range []struct{ arg, method, path string }{
		{"getthing", "DELETE", "/api/v1/thing"},
		{"GetThing", "GET", "/api/v1/thing"},
		{"get-thing", "GET", "/api/v1/thing"},
		{"list-items", "POST", "/api/v1/items"}, // exact operationId
		{"ListItems", "GET", "/api/v1/items"},
		{"list-items-get", "GET", "/api/v1/items"},
		{"get-health", "GET", "/api/v1/health"},
	} {
		want(t, e.run(t, c.arg), 0)
		if got := srv.lastCall(t); got.Method != c.method || got.Path != c.path {
			t.Fatalf("%s: %s %s", c.arg, got.Method, got.Path)
		}
	}
	want(t, e.run(t, "post-domains-id-notes", "--id", "d1"), 0)
	if got := srv.lastCall(t); got.Path != "/api/v1/domains/d1/notes" {
		t.Fatalf("path %s", got.Path)
	}
	for _, arg := range []string{"GETTHING", "List-Items"} {
		r := e.run(t, arg)
		want(t, r, 2)
		if !strings.Contains(r.Stderr, "matches several operations") {
			t.Fatalf("%s: %s", arg, r.Stderr)
		}
	}
}

// Item 10: location-qualified flags and an unambiguous body flag.
func TestQualifiedFlags(t *testing.T) {
	spec := `{"openapi":"3.0.3","paths":{"/items/{id}":{"post":{"operationId":"UpdateItem",
		"parameters":[{"name":"id","in":"path","required":true},{"name":"id","in":"query"},{"name":"body","in":"query"}],
		"requestBody":{"content":{"application/json":{"schema":{"properties":{"a":{}}}}}}}}}}`
	srv := specServer(t, spec)
	e := newEnv(t, srv.URL)
	want(t, e.run(t, "update-item", "--path.id", "1", "--query.id", "2", "--query.body", "x", "--request-body", `{"a":1}`), 0)
	if c := srv.lastCall(t); c.Path != "/api/v1/items/1" || c.RawQuery != "id=2&body=x" || string(c.Body) != `{"a":1}` {
		t.Fatalf("call %+v body %s", c, c.Body)
	}
	for _, args := range [][]string{
		{"update-item", "--id", "1"},
		{"update-item", "--path.id", "1", "--body", "{}"},
		{"update-item", "--path.id", "1", "--request-body", "{}", "--request-body", "{}"},
	} {
		r := e.run(t, args...)
		if r.Code != 2 {
			t.Fatalf("%v: exit %d", args, r.Code)
		}
	}
	if r := e.run(t, "update-item", "--id", "1"); !strings.Contains(r.Stderr, "use --path.id or --query.id") {
		t.Fatalf("stderr %s", r.Stderr)
	}
	r := e.run(t, "help", "update-item")
	want(t, r, 0)
	info := decode[map[string]any](t, r.Stdout)
	var flags []string
	for _, p := range info["parameters"].([]any) {
		flags = append(flags, p.(map[string]any)["flag"].(string))
	}
	if strings.Join(flags, " ") != "--path.id --query.id --query.body" || info["body_flag"] != "--request-body" {
		t.Fatalf("help: %v %v", flags, info["body_flag"])
	}
}

// Item 11: repeated flags for arrays, serialized by style and explode.
func TestArrayParameters(t *testing.T) {
	spec := `{"openapi":"3.0.3","paths":{"/search":{"get":{"operationId":"Search","parameters":[
		{"name":"tag","in":"query","schema":{"type":"array","items":{"type":"string"}}},
		{"name":"ids","in":"query","explode":false,"schema":{"type":"array"}},
		{"name":"pipes","in":"query","style":"pipeDelimited","explode":false,"schema":{"type":"array"}},
		{"name":"single","in":"query","schema":{"type":"string"}},
		{"name":"X-Ids","in":"header","schema":{"type":"array"}}]}}}}`
	srv := specServer(t, spec)
	e := newEnv(t, srv.URL)
	want(t, e.run(t, "search", "--tag", "a", "--tag", "b c", "--ids", "1", "--ids=2,3", "--pipes", "x", "--pipes", "y",
		"--single", "s", "--X-Ids", "7", "--X-Ids", "8"), 0)
	c := srv.lastCall(t)
	if c.RawQuery != "tag=a&tag=b+c&ids=1,2%2C3&pipes=x|y&single=s" {
		t.Fatalf("query %s", c.RawQuery)
	}
	want(t, e.run(t, "search", "--single", "a", "--single", "b"), 2)
	if r := e.run(t, "help", "search"); !strings.Contains(r.Stdout, `"repeatable": true`) {
		t.Fatalf("help %s", r.Stdout)
	}
}

// Item 12: cookie parameters are sent, or refused when they cannot be.
func TestCookieParameters(t *testing.T) {
	spec := `{"openapi":"3.0.3","paths":{"/me":{"get":{"operationId":"GetMe","parameters":[
		{"name":"session","in":"cookie","required":true},{"name":"theme","in":"cookie"}]}}}}`
	srv := specServer(t, spec)
	e := newEnv(t, srv.URL)
	want(t, e.run(t, "get-me", "--session", "abc", "--theme", "dark"), 0)
	if c := srv.lastCall(t); c.Cookie != "session=abc; theme=dark" {
		t.Fatalf("cookie %q", c.Cookie)
	}
	want(t, e.run(t, "get-me"), 2)
	for _, v := range []string{"a;b", "a b", `a"b`} {
		if r := e.run(t, "get-me", "--session", v); r.Code != 2 || !strings.Contains(r.Stderr, "cannot be sent as a cookie") {
			t.Fatalf("%q: exit %d stderr %s", v, r.Code, r.Stderr)
		}
	}
}

// Item 13: the declared JSON media type is sent and shown.
func TestJSONMediaTypes(t *testing.T) {
	spec := `{"openapi":"3.0.3","paths":{
		"/items/{id}":{"parameters":[{"name":"id","in":"path","required":true}],
			"patch":{"operationId":"PatchItem","requestBody":{"required":true,"content":{"application/merge-patch+json":{"schema":{"properties":{"name":{}}}}}}},
			"put":{"operationId":"PutItem","requestBody":{"content":{"application/merge-patch+json":{},"application/json":{"schema":{"properties":{"all":{}}}}}}},
			"post":{"operationId":"UploadItem","requestBody":{"content":{"multipart/form-data":{}}}},
			"delete":{"operationId":"DeleteItem"}}}}`
	srv := specServer(t, spec)
	e := newEnv(t, srv.URL)
	want(t, e.run(t, "patch-item", "--id", "1", "--body", `{"name":"n"}`), 0)
	if c := srv.lastCall(t); c.ContentType != "application/merge-patch+json" {
		t.Fatalf("content type %q", c.ContentType)
	}
	want(t, e.run(t, "put-item", "--id", "1", "--body", `{}`), 0)
	if c := srv.lastCall(t); c.ContentType != "application/json" {
		t.Fatalf("content type %q", c.ContentType)
	}
	r := e.run(t, "help", "patch-item")
	info := decode[map[string]any](t, r.Stdout)
	if info["body_media_type"] != "application/merge-patch+json" || fmt.Sprint(info["body_fields"]) != "[name]" || info["body_needed"] != true {
		t.Fatalf("help %v", info)
	}
	calls := srv.callCount()
	for _, args := range [][]string{
		{"patch-item", "--id", "1"}, // required body missing
		{"upload-item", "--id", "1", "--body", "{}"},
		{"delete-item", "--id", "1", "--body", "{}"},
	} {
		if r := e.run(t, args...); r.Code != 2 {
			t.Fatalf("%v: exit %d", args, r.Code)
		}
	}
	if srv.callCount() != calls {
		t.Fatal("a refused body reached the server")
	}
	r = e.run(t, "help", "upload-item")
	if !strings.Contains(decode[map[string]any](t, r.Stdout)["unsupported"].(string), "[multipart/form-data] are not supported") {
		t.Fatalf("help %s", r.Stdout)
	}
}

// Item 14: release archives carry the Go licence; the copy must match the
// toolchain's.
func TestGoLicenceIsBundled(t *testing.T) {
	out, err := exec.Command("go", "env", "GOROOT").Output()
	if err != nil {
		t.Skip("no go command")
	}
	goLicense, err := os.ReadFile(filepath.Join(strings.TrimSpace(string(out)), "LICENSE"))
	if err != nil {
		t.Fatal(err)
	}
	bundled, err := os.ReadFile(filepath.Join("..", "..", "licenses", "go", "LICENSE"))
	if err != nil || !bytes.Equal(bytes.ReplaceAll(bundled, []byte("\r\n"), []byte("\n")), bytes.ReplaceAll(goLicense, []byte("\r\n"), []byte("\n"))) {
		t.Fatalf("licenses/go/LICENSE differs from the Go toolchain's LICENSE (%v)", err)
	}
	cfg, err := os.ReadFile(filepath.Join("..", "..", ".goreleaser.yaml"))
	if err != nil || !bytes.Contains(cfg, []byte("licenses/go/LICENSE")) {
		t.Fatal(".goreleaser.yaml does not put licenses/go/LICENSE in the archives")
	}
	notice, err := os.ReadFile(filepath.Join("..", "..", "NOTICE"))
	if err != nil || !bytes.Contains(notice, []byte("licenses/go/LICENSE")) {
		t.Fatal("NOTICE does not mention the Go licence")
	}
}

// Items 15 and 16: output that cannot be written is an error exit.
func TestWriteFailuresExitNonzero(t *testing.T) {
	full, err := os.OpenFile("/dev/full", os.O_WRONLY, 0)
	if err != nil {
		t.Skip("no /dev/full")
	}
	defer func() { _ = full.Close() }()
	srv := newFakeServer(t, false)
	e := newEnv(t, srv.URL)
	e.stdout = full
	for _, args := range [][]string{{"version"}, {"--help"}, {"help"}, {"operations"}, {"help", "get-domain"}, {"get-domain", "--domainId", "d"}} {
		r := e.run(t, args...)
		if r.Code != 1 || !strings.Contains(r.Stderr, "cannot write output") {
			t.Fatalf("%v: exit %d stderr %s", args, r.Code, r.Stderr)
		}
	}
}
