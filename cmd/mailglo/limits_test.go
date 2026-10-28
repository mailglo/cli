package main

// End-to-end tests for round five: a loader panic is an invalid
// description, and output is capped.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"unicode/utf8"
)

// panicSpec makes kin-openapi v0.142.0 panic while loading: a reference
// into a schema that is itself a reference.
const panicSpec = `{"openapi":"3.0.3","info":{"title":"t","version":"1"},"components":{"schemas":{
	"A":{"$ref":"#/components/schemas/B/additionalProperties"},
	"B":{"$ref":"#/components/schemas/C"},
	"C":{"type":"object","additionalProperties":{"type":"string"}}}},
	"paths":{"/x":{"get":{"operationId":"X","responses":{"200":{"description":"OK",
		"content":{"application/json":{"schema":{"$ref":"#/components/schemas/A"}}}}}}}}}`

func TestLoaderPanicIsAnInvalidDescription(t *testing.T) {
	if _, err := parseSpec([]byte(panicSpec)); err == nil || !strings.Contains(err.Error(), "the loader failed on it") {
		t.Fatalf("parse: %v", err)
	}

	// No cache: exit 6 with the reason.
	r := newEnv(t, specServer(t, panicSpec).URL).run(t, "operations")
	want(t, r, 6)
	if !strings.Contains(r.Stderr, "invalid OpenAPI description: the loader failed on it") || strings.Contains(r.Stderr, "goroutine") {
		t.Fatalf("stderr %s", r.Stderr)
	}

	// With a cache: the cached copy is used, with a warning.
	srv := newFakeServer(t, false)
	e := newEnv(t, srv.URL)
	want(t, e.run(t, "operations"), 0)
	srv.set(func() { srv.spec, srv.etag = []byte(panicSpec), `"broken"` })
	r = e.run(t, "operations")
	want(t, r, 0)
	if !strings.Contains(r.Stderr, "using the copy cached") || !strings.Contains(r.Stdout, "list-domains") {
		t.Fatalf("stdout %.200s stderr %s", r.Stdout, r.Stderr)
	}
}

func TestOutputTextIsCapped(t *testing.T) {
	long := strings.Repeat("é", 60000) // 120 KB, two bytes per rune
	spec, _ := json.Marshal(map[string]any{"openapi": "3.0.3", "paths": map[string]any{"/x": map[string]any{"get": map[string]any{
		"operationId": "X", "summary": long,
		"parameters": []any{map[string]any{"name": "q", "in": "query", "description": long}}}}}})
	e := newEnv(t, specServer(t, string(spec)).URL)
	capped := func(s string) {
		t.Helper()
		if len(s) > maxTextBytes || !strings.HasSuffix(s, "…") || !utf8.ValidString(s) || !strings.HasPrefix(long, strings.TrimSuffix(s, "…")) {
			t.Fatalf("text of %d bytes: %.40q", len(s), s)
		}
	}
	r := e.run(t, "operations")
	want(t, r, 0)
	capped(decode[[]map[string]string](t, r.Stdout)[0]["summary"])
	r = e.run(t, "help", "x")
	want(t, r, 0)
	info := decode[map[string]any](t, r.Stdout)
	capped(info["summary"].(string))
	capped(info["parameters"].([]any)[0].(map[string]any)["description"].(string))
}

func TestOutputBudget(t *testing.T) {
	shared := func(n int) string {
		paths := map[string]any{"/p0": map[string]any{"get": map[string]any{"summary": strings.Repeat("s", 2000)}}}
		for i := 1; i < n; i++ {
			paths[fmt.Sprintf("/p%d", i)] = map[string]any{"$ref": "#/paths/~1p0"}
		}
		b, _ := json.Marshal(map[string]any{"openapi": "3.0.3", "paths": paths})
		return string(b)
	}

	// 201 paths sharing one summary print one capped copy each.
	r := newEnv(t, specServer(t, shared(201)).URL).run(t, "operations")
	want(t, r, 0)
	if n := len(decode[[]map[string]string](t, r.Stdout)); n != 201 || len(r.Stdout) > 400<<10 {
		t.Fatalf("%d operations in %d bytes", n, len(r.Stdout))
	}

	// 9,000 of them would pass the 8 MiB budget: refused before encoding.
	r = newEnv(t, specServer(t, shared(9000)).URL).run(t, "operations")
	want(t, r, 1)
	if r.Stdout != "" || !strings.Contains(r.Stderr, "the operation list would be larger than 8 MiB") {
		t.Fatalf("stdout %d bytes, stderr %s", len(r.Stdout), r.Stderr)
	}

	// Help for an operation whose flags alone pass the budget.
	var params []any
	for i := 0; i < 2100; i++ {
		params = append(params, map[string]any{"name": fmt.Sprintf("p%d-%s", i, strings.Repeat("n", 2000)), "in": "query"})
	}
	b, _ := json.Marshal(map[string]any{"openapi": "3.0.3", "paths": map[string]any{"/w": map[string]any{"get": map[string]any{"operationId": "W", "parameters": params}}}})
	r = newEnv(t, specServer(t, string(b)).URL).run(t, "help", "w")
	want(t, r, 1)
	if r.Stdout != "" || !strings.Contains(r.Stderr, "help for w would be larger than 8 MiB") {
		t.Fatalf("stdout %d bytes, stderr %s", len(r.Stdout), r.Stderr)
	}
}

// The streaming encoder prints exactly what json.Encoder would, with C1
// characters escaped.
func TestStreamingEncoderMatchesEncodingJSON(t *testing.T) {
	values := []any{
		map[string]any{"a": "x y\u0085z<&>", "b": true, "c": []map[string]any{{"k": "v", "n": 1}, {}},
			"d": []string(nil), "e": []string{}, "f": []string{"p", "q"}, "g": map[string]any{}, "h": nil},
		[]map[string]string{{"operation": "x", "summary": "\x1b]52;c;\x07"}, {"z": ""}},
		[]map[string]string{},
		map[string]string{"version": "1"},
	}
	for _, v := range values {
		var want bytes.Buffer
		enc := json.NewEncoder(&want)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if err := enc.Encode(v); err != nil {
			t.Fatal(err)
		}
		var got bytes.Buffer
		if code := emitLimited(&got, io.Discard, v, "test"); code != 0 {
			t.Fatalf("exit %d", code)
		}
		if got.String() != string(escapeJSON(want.Bytes())) {
			t.Fatalf("got\n%s\nwant\n%s", got.String(), escapeJSON(want.Bytes()))
		}
	}
}

// Round 6: the 8 MiB limit is enforced on the encoded bytes.
func TestOutputLimitIsExact(t *testing.T) {
	t.Run("a 9 MiB media type is cut to 1 KB", func(t *testing.T) {
		mt := "application/" + strings.Repeat("x", 9<<20) + "+json"
		spec, _ := json.Marshal(map[string]any{"openapi": "3.0.3", "paths": map[string]any{"/x": map[string]any{"post": map[string]any{
			"operationId": "X", "requestBody": map[string]any{"content": map[string]any{mt: map[string]any{}}}}}}})
		r := newEnv(t, specServer(t, string(spec)).URL).run(t, "help", "x")
		want(t, r, 0)
		got := decode[map[string]any](t, r.Stdout)["body_media_type"].(string)
		if len(got) > maxTextBytes || !strings.HasSuffix(got, "…") || len(r.Stdout) > 64<<10 {
			t.Fatalf("media type of %d bytes in %d bytes of output", len(got), len(r.Stdout))
		}
	})

	t.Run("U+2028 escapes to six bytes", func(t *testing.T) {
		summary := strings.Repeat(" ", 400) // 1,200 bytes, cut to 1 KB, printed as about 2 KB
		paths := map[string]any{"/p0": map[string]any{"get": map[string]any{"summary": summary}}}
		for i := 1; i < 5000; i++ {
			paths[fmt.Sprintf("/p%d", i)] = map[string]any{"$ref": "#/paths/~1p0"}
		}
		spec, _ := json.Marshal(map[string]any{"openapi": "3.0.3", "paths": paths})
		r := newEnv(t, specServer(t, string(spec)).URL).run(t, "operations")
		want(t, r, 1)
		if r.Stdout != "" || !strings.Contains(r.Stderr, "the operation list would be larger than 8 MiB") {
			t.Fatalf("stdout %d bytes, stderr %s", len(r.Stdout), r.Stderr)
		}
	})

	t.Run("56,000 scalar query parameters", func(t *testing.T) {
		if raceEnabled {
			t.Skip("validating 56,000 parameters under the race detector takes minutes")
		}
		var params []any
		for i := 0; i < 56000; i++ {
			params = append(params, map[string]any{"name": fmt.Sprintf("q%d", i), "in": "query", "schema": map[string]any{"type": "string"}})
		}
		spec, _ := json.Marshal(map[string]any{"openapi": "3.0.3", "paths": map[string]any{"/w": map[string]any{"get": map[string]any{
			"operationId": "W", "parameters": params}}}})
		r := newEnv(t, specServer(t, string(spec)).URL).run(t, "help", "w")
		want(t, r, 1)
		if r.Stdout != "" || !strings.Contains(r.Stderr, "help for w would be larger than 8 MiB") {
			t.Fatalf("stdout %d bytes, stderr %s", len(r.Stdout), r.Stderr)
		}
	})
}
