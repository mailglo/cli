package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// This file reads the parts of an OpenAPI 3 description (JSON) that the CLI
// needs: each operation's method, path, parameters and JSON request body,
// and the API base path from "servers". Local references ("#/...") are
// followed; nothing else is validated, because the server is the authority
// on what it accepts.

type api struct {
	Ops []*operation // sorted by Name
	// ServerURL is the first "servers" entry with its variables filled
	// in, or "" when the description declares none.
	ServerURL string
	byName    map[string]*operation
}

type operation struct {
	Name                  string // command name, unique within the description
	ID                    string // operationId; may be empty
	Method, Path, Summary string
	Params                []param
	Body                  *body
}

type param struct {
	Name, In, Description string
	Required              bool
	Array                 bool
	Style                 string
	Explode               bool
}

type body struct {
	Required       bool     // the operation needs a body
	MediaType      string   // JSON media type to send; "" when none is declared
	Declared       []string // all declared media types, sorted
	Fields         []string // top-level fields, sorted
	RequiredFields []string // required top-level fields, sorted
}

var methods = []string{"get", "put", "post", "delete", "options", "head", "patch", "trace"}

// maxSteps bounds the work spent following references and schemas, so a
// hostile description cannot make the CLI spin.
const maxSteps = 1_000_000

var errTooComplex = errors.New("description is too complex")

// parseSpec reads an OpenAPI description. The result does not depend on map
// iteration order.
func parseSpec(raw []byte) (*api, error) {
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, err
	}
	paths, ok := root["paths"].(map[string]any)
	if !ok {
		return nil, errors.New("no paths")
	}
	r := &resolver{root: root, steps: maxSteps}
	a := &api{ServerURL: serverURL(root)}
	for _, path := range sortedKeys(paths) {
		item := r.object(paths[path])
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
			summary, _ := op["summary"].(string)
			a.Ops = append(a.Ops, &operation{ID: id, Method: strings.ToUpper(m), Path: path, Summary: summary,
				Params: merge(shared, r.params(op["parameters"])), Body: r.body(op["requestBody"])})
		}
	}
	if r.steps < 0 {
		return nil, errTooComplex
	}
	a.name()
	return a, nil
}

// name gives every operation a unique command name. It is the kebab-case
// operationId, or method and path when there is none. Operations whose names
// would clash get the method appended, then a number.
func (a *api) name() {
	base := map[*operation]string{}
	count := map[string]int{}
	for _, o := range a.Ops {
		b := commandName(o.ID)
		if b == "" {
			b = commandName(strings.ToLower(o.Method) + "-" + o.Path)
		}
		base[o] = b
		count[b]++
	}
	sort.SliceStable(a.Ops, func(i, j int) bool {
		x, y := a.Ops[i], a.Ops[j]
		if base[x] != base[y] {
			return base[x] < base[y]
		}
		if x.Method != y.Method {
			return x.Method < y.Method
		}
		if x.Path != y.Path {
			return x.Path < y.Path
		}
		return x.ID < y.ID
	})
	used := map[string]bool{}
	for _, o := range a.Ops { // unique names first, so a suffixed name cannot take one
		if count[base[o]] == 1 {
			o.Name = base[o]
			used[o.Name] = true
		}
	}
	for _, o := range a.Ops {
		if o.Name != "" {
			continue
		}
		cand := base[o] + "-" + strings.ToLower(o.Method)
		name := cand
		for n := 2; used[name]; n++ {
			name = fmt.Sprintf("%s-%d", cand, n)
		}
		o.Name = name
		used[name] = true
	}
	sort.Slice(a.Ops, func(i, j int) bool { return a.Ops[i].Name < a.Ops[j].Name })
	a.byName = map[string]*operation{}
	for _, o := range a.Ops {
		a.byName[o.Name] = o
	}
}

// lookup finds an operation by command name, then by exact operationId,
// then case-insensitively when that is unambiguous.
func (a *api) lookup(s string) (*operation, error) {
	if o := a.byName[s]; o != nil {
		return o, nil
	}
	var exact, folded []*operation
	for _, o := range a.Ops {
		switch {
		case o.ID != "" && o.ID == s:
			exact = append(exact, o)
		case strings.EqualFold(o.Name, s) || (o.ID != "" && (strings.EqualFold(o.ID, s) || strings.EqualFold(kebab(o.ID), s))):
			folded = append(folded, o)
		}
	}
	for _, list := range [][]*operation{exact, folded} {
		switch len(list) {
		case 0:
			continue
		case 1:
			return list[0], nil
		}
		var names []string
		for _, o := range list {
			names = append(names, o.Name)
		}
		return nil, fmt.Errorf("%q matches several operations: %s", s, strings.Join(names, ", "))
	}
	return nil, fmt.Errorf("unknown operation %q; run: mailglo operations", s)
}

// commandName turns an operationId, or "method-/path/{param}", into a
// lower-case name made of letters, digits, '-', '_' and '.'.
func commandName(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range kebab(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '.':
			b.WriteRune(r)
			dash = false
		case !dash && b.Len() > 0:
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.TrimRight(b.String(), "-")
}

// serverURL returns the first server URL with its variables set to their
// defaults.
func serverURL(root map[string]any) string {
	list, _ := root["servers"].([]any)
	if len(list) == 0 {
		return ""
	}
	s, _ := list[0].(map[string]any)
	u, _ := s["url"].(string)
	vars, _ := s["variables"].(map[string]any)
	for _, name := range sortedKeys(vars) {
		if m, ok := vars[name].(map[string]any); ok {
			if def, ok := m["default"].(string); ok {
				u = strings.ReplaceAll(u, "{"+name+"}", def)
			}
		}
	}
	return u
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

type resolver struct {
	root  map[string]any
	steps int
}

// object follows local $ref chains and returns the JSON object they end at.
func (r *resolver) object(v any) map[string]any {
	for depth := 0; depth < 32; depth++ {
		if r.steps--; r.steps < 0 {
			return nil
		}
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

func (r *resolver) pointer(ref string) any {
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

func (r *resolver) params(v any) []param {
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
		style, _ := p["style"].(string)
		if style == "" {
			style = "simple"
			if in == "query" || in == "cookie" {
				style = "form"
			}
		}
		explode, ok := p["explode"].(bool)
		if !ok {
			explode = style == "form"
		}
		out = append(out, param{Name: name, In: in, Description: desc, Required: req || in == "path",
			Array: r.isArray(p["schema"]), Style: style, Explode: explode})
	}
	return out
}

func (r *resolver) isArray(v any) bool {
	s := r.object(v)
	switch t := s["type"].(type) {
	case string:
		return t == "array"
	case []any: // OpenAPI 3.1 type lists
		for _, x := range t {
			if x == "array" {
				return true
			}
		}
	}
	return false
}

// jsonMediaType reports whether a media type is JSON: application/json or
// any type with a +json suffix, such as application/merge-patch+json.
func jsonMediaType(mt string) bool {
	mt = strings.ToLower(strings.TrimSpace(strings.SplitN(mt, ";", 2)[0]))
	return mt == "application/json" || strings.HasSuffix(mt, "+json")
}

func (r *resolver) body(v any) *body {
	rb := r.object(v)
	if rb == nil {
		return nil
	}
	b := &body{}
	b.Required, _ = rb["required"].(bool)
	content := r.object(rb["content"])
	b.Declared = sortedKeys(content)
	for _, mt := range b.Declared {
		if jsonMediaType(mt) && (b.MediaType == "" || strings.EqualFold(mt, "application/json")) {
			b.MediaType = mt
		}
	}
	if b.MediaType == "" {
		return b
	}
	fields, required := map[string]bool{}, map[string]bool{}
	r.collect(r.object(content[b.MediaType])["schema"], fields, required, map[string]bool{})
	b.Fields, b.RequiredFields = sortedKeys(fields), sortedKeys(required)
	return b
}

// collect gathers top-level properties, including those combined with allOf.
// Each referenced schema is visited once, so repeated or cyclic references
// cost nothing extra.
func (r *resolver) collect(v any, fields, required map[string]bool, seen map[string]bool) {
	if m, ok := v.(map[string]any); ok {
		if ref, ok := m["$ref"].(string); ok {
			if seen[ref] {
				return
			}
			seen[ref] = true
		}
	}
	s := r.object(v)
	if s == nil {
		return
	}
	for k := range r.object(s["properties"]) {
		fields[k] = true
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
			r.collect(sub, fields, required, seen)
		}
	}
}

func sortedKeys[V any](m map[string]V) []string {
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
		return fmt.Errorf("not a usable OpenAPI description: %w", err)
	}
	return nil
}
