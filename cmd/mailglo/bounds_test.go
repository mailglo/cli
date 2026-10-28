package main

// End-to-end tests for round-three limits: plain-http tokens, server
// variable expansion, wide help and the cache size.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// Item 1: a token goes over plain http only to localhost or a literal
// loopback address, directly and never through a proxy.
func TestPlainHTTPTokenStaysLocal(t *testing.T) {
	var mu sync.Mutex
	var proxied []string
	srv := newFakeServer(t, false)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		proxied = append(proxied, r.Method+" "+r.URL.String()+" auth="+r.Header.Get("Authorization"))
		mu.Unlock()
		srv.Config.Handler.ServeHTTP(w, r) // answer like the real server
	}))
	t.Cleanup(proxy.Close)
	seen := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), proxied...)
	}
	port := srv.URL[strings.LastIndex(srv.URL, ":"):]
	withProxy := func(e *env) *env {
		for _, k := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy"} {
			e.vars[k] = proxy.URL
		}
		e.vars["NO_PROXY"], e.vars["no_proxy"] = "", ""
		return e
	}

	// The proxy is honoured for other hosts, so the checks below are real.
	want(t, withProxy(newEnv(t, "http://mailglo.example")).run(t, "get-domain", "--domainId", "d"), 0)
	if got := seen(); len(got) != 2 || !strings.Contains(got[1], "http://mailglo.example/api/v1/domains/d") {
		t.Fatalf("proxy saw %v", got)
	}

	// Neither the description nor the API request to this machine goes
	// through the proxy, so the proxy can neither read the token nor
	// change the description.
	for _, server := range []string{"http://localhost" + port, "http://LOCALHOST" + port, srv.URL} {
		for _, token := range []string{"", testToken} {
			e := withProxy(newEnv(t, server))
			if token != "" {
				e.vars["MAILGLO_TOKEN"] = token
			}
			before := len(seen())
			want(t, e.run(t, "get-domain", "--domainId", "d"), 0)
			if c := srv.lastCall(t); token != "" && c.Auth != "Bearer "+token {
				t.Fatalf("%s: auth %q", server, c.Auth)
			}
			if got := seen()[before:]; len(got) != 0 {
				t.Fatalf("%s: went through the proxy: %v", server, got)
			}
		}
	}

	for _, server := range []string{"http://sub.localhost" + port, "http://localhost.example" + port, "http://127.0.0.1.nip.example" + port} {
		e := withProxy(newEnv(t, server))
		e.vars["MAILGLO_TOKEN"] = testToken
		before := len(seen())
		r := e.run(t, "get-domain", "--domainId", "d")
		want(t, r, 2)
		if !strings.Contains(r.Stderr, "refusing to send MAILGLO_TOKEN") || len(seen()) != before {
			t.Fatalf("%s: stderr %s", server, r.Stderr)
		}
	}
}

// Item 2: server variables expand once, within a length limit.
func TestServerVariablesAreBounded(t *testing.T) {
	var url strings.Builder
	vars := map[string]any{}
	for i := 0; i < 30; i++ {
		fmt.Fprintf(&url, "{v%d}", i)
		vars[fmt.Sprintf("v%d", i)] = map[string]any{"default": strings.Repeat("a", 100)}
	}
	spec, _ := json.Marshal(map[string]any{"openapi": "3.0.3",
		"servers": []any{map[string]any{"url": "/" + url.String(), "variables": vars}},
		"paths":   map[string]any{"/x": map[string]any{"get": map[string]any{"operationId": "X"}}}})
	r := newEnv(t, specServer(t, string(spec)).URL).run(t, "x")
	want(t, r, 2)
	if !strings.Contains(r.Stderr, "longer than 2048 bytes") {
		t.Fatalf("stderr %s", r.Stderr)
	}

	// 100,000 uses of a 100 KB default would expand to 10 GB; the limit
	// stops the expansion as soon as it is passed. (At the document level
	// the validator already refuses a variable used twice, so this uses an
	// operation's servers.)
	spec, _ = json.Marshal(map[string]any{"openapi": "3.0.3",
		"paths": map[string]any{"/x": map[string]any{"get": map[string]any{"operationId": "X",
			"servers": []any{map[string]any{"url": "/" + strings.Repeat("{v}", 100000),
				"variables": map[string]any{"v": map[string]any{"default": strings.Repeat("a", 100000)}}}}}}}})
	r = newEnv(t, specServer(t, string(spec)).URL).run(t, "x")
	want(t, r, 2)
	if !strings.Contains(r.Stderr, "longer than 2048 bytes") {
		t.Fatalf("stderr %s", r.Stderr)
	}

	// A default is not expanded again.
	srv := specServer(t, `{"openapi":"3.0.3","servers":[{"url":"/{a}/{b}","variables":{"a":{"default":"v1"},"b":{"default":"x"}}}],
		"paths":{"/things":{"get":{"operationId":"Fetch"}}}}`)
	want(t, newEnv(t, srv.URL).run(t, "fetch"), 0)
	if c := srv.lastCall(t); c.Path != "/v1/x/things" {
		t.Fatalf("path %s", c.Path)
	}
}

// Item 3: help for a very wide operation is linear.
func TestWideHelpIsFast(t *testing.T) {
	var params []any
	n := size(40000)
	for i := 0; i < n; i++ {
		params = append(params, map[string]any{"name": fmt.Sprintf("p%d", i), "in": "query"})
	}
	params = append(params, map[string]any{"name": "p1", "in": "header"}) // one ambiguous name
	spec, _ := json.Marshal(map[string]any{"openapi": "3.0.3",
		"paths": map[string]any{"/wide": map[string]any{"get": map[string]any{"operationId": "Wide", "parameters": params}}}})
	srv := specServer(t, string(spec))
	e := newEnv(t, srv.URL)
	want(t, e.run(t, "operations"), 0) // fetch and cache
	start := time.Now()
	r := e.run(t, "help", "wide")
	elapsed := time.Since(start)
	want(t, r, 0)
	if !raceEnabled && elapsed > 5*time.Second {
		t.Fatalf("help took %s", elapsed)
	}
	info := decode[map[string]any](t, r.Stdout)
	ps := info["parameters"].([]any)
	if len(ps) != n+1 || ps[1].(map[string]any)["flag"] != "--query.p1" || ps[2].(map[string]any)["flag"] != "--p2" {
		t.Fatalf("flags: %v %v", ps[1], ps[2])
	}
	last := fmt.Sprintf("p%d", n-1)
	want(t, e.run(t, "wide", "--"+last, "z", "--header.p1", "h"), 0)
	if c := srv.lastCall(t); c.RawQuery != last+"=z" {
		t.Fatalf("query %s", c.RawQuery)
	}
}

// Item 8: the cache stores the description as sent, so anything the CLI
// accepts can be read back for offline use.
func TestCacheStoresDescriptionVerbatim(t *testing.T) {
	summary := strings.Repeat("<&>", 400000) // escaped as \u003c and so on, this would grow sixfold
	spec := `{"openapi":"3.0.3","paths":{"/x":{"get":{"operationId":"X","summary":"` + summary + `"}}}}`
	srv := specServer(t, spec)
	e := newEnv(t, srv.URL)
	want(t, e.run(t, "operations"), 0)
	fi, err := os.Stat(cacheFileIn(t, e.cache, srv.URL))
	if err != nil || fi.Size() > int64(len(spec))+4096 {
		t.Fatalf("cache entry is %d bytes for a %d-byte description (%v)", fi.Size(), len(spec), err)
	}
	srv.set(func() { srv.specStatus = http.StatusServiceUnavailable })
	r := e.run(t, "help", "x")
	want(t, r, 0)
	if decode[map[string]any](t, r.Stdout)["summary"] != summary {
		t.Fatal("the cached description differs")
	}
	if maxCacheBytes < maxSpecBytes+maxMetaBytes {
		t.Fatal("the cache read limit is below the largest entry the CLI writes")
	}
}
