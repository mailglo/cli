package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// This file reads the parts of an OpenAPI 3 description (JSON) that the CLI
// needs: each operation's method, path, parameters and JSON body fields.
// Local references ("#/components/...") are followed; nothing else is
// validated, because the server is the authority on what it accepts.

type operation struct {
	ID, Method, Path, Summary string
	Params                    []param
	Body                      *body
}

type param struct {
	Name, In, Description string
	Required              bool
}

type body struct {
	Required []string // required top-level fields
	Fields   []string // all top-level fields, sorted
}

var methods = []string{"get", "put", "post", "delete", "options", "head", "patch", "trace"}

// parseSpec returns the operations of an OpenAPI description, keyed by the
// lower-case operationId and by its kebab-case form.
func parseSpec(raw []byte) (map[string]*operation, error) {
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, err
	}
	paths, ok := root["paths"].(map[string]any)
	if !ok {
		return nil, errors.New("no paths")
	}
	r := resolver{root: root}
	out := map[string]*operation{}
	for path, v := range paths {
		item := r.object(v)
		if item == nil {
			continue
		}
		shared := r.params(item["parameters"])
		for _, m := range methods {
			op := r.object(item[m])
			if op == nil {
				continue
			}
			id, _ := op["operationId"].(string)
			if id == "" {
				continue
			}
			summary, _ := op["summary"].(string)
			o := &operation{ID: id, Method: strings.ToUpper(m), Path: path, Summary: summary,
				Params: merge(shared, r.params(op["parameters"])), Body: r.body(op["requestBody"])}
			out[strings.ToLower(id)] = o
			out[strings.ToLower(kebab(id))] = o
		}
	}
	return out, nil
}

// merge lets operation parameters override path-level ones with the same
// name and location.
func merge(shared, own []param) []param {
	out := append([]param(nil), own...)
	for _, s := range shared {
		dup := false
		for _, p := range own {
			if p.Name == s.Name && p.In == s.In {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, s)
		}
	}
	return out
}

type resolver struct{ root map[string]any }

// object follows local $ref chains and returns the JSON object they end at.
func (r resolver) object(v any) map[string]any {
	for depth := 0; depth < 32; depth++ {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		ref, ok := m["$ref"].(string)
		if !ok {
			return m
		}
		v = r.pointer(ref)
	}
	return nil
}

func (r resolver) pointer(ref string) any {
	rest, ok := strings.CutPrefix(ref, "#/")
	if !ok {
		return nil
	}
	var cur any = r.root
	for _, part := range strings.Split(rest, "/") {
		part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[part]
	}
	return cur
}

func (r resolver) params(v any) []param {
	list, _ := v.([]any)
	var out []param
	for _, item := range list {
		p := r.object(item)
		if p == nil {
			continue
		}
		name, _ := p["name"].(string)
		in, _ := p["in"].(string)
		if name == "" || in == "" {
			continue
		}
		desc, _ := p["description"].(string)
		req, _ := p["required"].(bool)
		out = append(out, param{Name: name, In: in, Description: desc, Required: req || in == "path"})
	}
	return out
}

func (r resolver) body(v any) *body {
	rb := r.object(v)
	if rb == nil {
		return nil
	}
	content := r.object(rb["content"])
	mt := r.object(content["application/json"])
	if mt == nil {
		return nil
	}
	fields, required := map[string]bool{}, map[string]bool{}
	r.collect(mt["schema"], fields, required, 0)
	b := &body{Required: keys(required), Fields: keys(fields)}
	return b
}

// collect gathers top-level properties, including those combined with allOf.
func (r resolver) collect(v any, fields, required map[string]bool, depth int) {
	s := r.object(v)
	if s == nil || depth > 16 {
		return
	}
	if props := r.object(s["properties"]); props != nil {
		for k := range props {
			fields[k] = true
		}
	}
	if req, ok := s["required"].([]any); ok {
		for _, k := range req {
			if name, ok := k.(string); ok {
				required[name] = true
			}
		}
	}
	if all, ok := s["allOf"].([]any); ok {
		for _, sub := range all {
			r.collect(sub, fields, required, depth+1)
		}
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// validSpec reports whether raw is an OpenAPI description the CLI can use.
func validSpec(raw []byte) error {
	if _, err := parseSpec(raw); err != nil {
		return fmt.Errorf("not an OpenAPI description: %w", err)
	}
	return nil
}
