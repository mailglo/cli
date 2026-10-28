package main

// End-to-end tests for round-four limits on top of kin-openapi: size,
// long operationIds and canonical header names.

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestDescriptionSizeLimit(t *testing.T) {
	pad := strings.Repeat("a", maxSpecBytes)
	spec := `{"openapi":"3.0.3","info":{"title":"t","version":"1","description":"` + pad + `"},"paths":{}}`
	r := newEnv(t, specServer(t, spec).URL).run(t, "operations")
	want(t, r, 6)
	if !strings.Contains(r.Stderr, fmt.Sprintf("larger than %d bytes", maxSpecBytes)) {
		t.Fatalf("stderr %s", r.Stderr)
	}
}

// Long operationIds give names of at most maxNameBytes, and a shared one is
// refused by validation rather than expanded.
func TestLongOperationIDs(t *testing.T) {
	paths := map[string]any{}
	for i := 0; i < 50; i++ {
		paths[fmt.Sprintf("/p%d", i)] = map[string]any{"get": map[string]any{"operationId": fmt.Sprintf("Op%d", i) + strings.Repeat("Ab", 50000)}}
	}
	raw, _ := json.Marshal(map[string]any{"openapi": "3.0.3", "paths": paths})
	srv := specServer(t, string(raw))
	e := newEnv(t, srv.URL)
	r := e.run(t, "operations")
	want(t, r, 0)
	var name string
	for _, o := range decode[[]map[string]string](t, r.Stdout) {
		if len(o["operation"]) > maxNameBytes {
			t.Fatalf("name of %d bytes", len(o["operation"]))
		}
		if o["path"] == "/p7" {
			name = o["operation"]
		}
	}
	want(t, e.run(t, name), 0)
	if c := srv.lastCall(t); c.Path != "/api/v1/p7" {
		t.Fatalf("path %s", c.Path)
	}

	// 3000 path items sharing one operation, and so one operationId.
	paths = map[string]any{"/p0": map[string]any{"get": map[string]any{"operationId": strings.Repeat("A", 100000)}}}
	for i := 1; i < 3000; i++ {
		paths[fmt.Sprintf("/p%d", i)] = map[string]any{"$ref": "#/paths/~1p0"}
	}
	raw, _ = json.Marshal(map[string]any{"openapi": "3.0.3", "paths": paths})
	r = newEnv(t, specServer(t, string(raw)).URL).run(t, "operations")
	want(t, r, 6)
	if !strings.Contains(r.Stderr, "invalid OpenAPI description") {
		t.Fatalf("stderr %.300s", r.Stderr)
	}
}

// Header parameters are matched on their canonical names, for duplicates
// and for operation-level overrides.
func TestCanonicalHeaderNames(t *testing.T) {
	spec := `{"openapi":"3.0.3","paths":{"/x":{
		"parameters":[{"name":"X-Name","in":"header","description":"path level"}],
		"get":{"operationId":"X","parameters":[{"name":"x-name","in":"header","description":"operation level"}]}}}}`
	srv := specServer(t, spec)
	e := newEnv(t, srv.URL)
	info := decode[map[string]any](t, e.run(t, "help", "x").Stdout)
	ps := info["parameters"].([]any)
	if len(ps) != 1 || ps[0].(map[string]any)["description"] != "operation level" {
		t.Fatalf("parameters %v", ps)
	}
	want(t, e.run(t, "x", "--x-name", "v"), 0)
}
