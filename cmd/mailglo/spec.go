package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// This file reads an OpenAPI description (JSON) into the operations the CLI
// can call. It accepts the subset documented under "Supported API
// descriptions" in README.md. A document outside it is refused; an
// operation outside it is kept, marked unsupported with the construct that
// put it there, and refused when someone runs it.

type api struct {
	Ops    []*operation // sorted by Name
	byName map[string]*operation
}

type operation struct {
	Name                  string // command name, unique within the description
	ID                    string // operationId; may be empty
	Method, Path, Summary string
	ServerURL             string // effective first server URL; "" when none is declared
	Params                []param
	Body                  *body
	Unsupported           string // why the CLI cannot call this operation, or ""
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
	MediaType      string   // the media type the CLI sends
	JSON           bool     // MediaType is JSON; otherwise the body is sent as raw bytes
	Declared       []string // all declared media types, sorted
	Fields         []string // top-level JSON fields, sorted
	RequiredFields []string // required top-level JSON fields, sorted
}

var methods = []string{"get", "put", "post", "delete", "options", "head", "patch", "trace"}

// builtins are CLI commands; no operation is named after one.
var builtins = []string{"help", "operations", "version"}

// maxSteps bounds the work spent on one description (references,
// schemas, parameters and naming), so a hostile one cannot make the CLI spin.
const maxSteps = 1_000_000

var errTooComplex = errors.New("description is too complex")

// parseSpec reads an OpenAPI description. The result does not depend on map
// iteration order.
func parseSpec(raw []byte) (*api, error) {
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, err
	}
	v, _ := root["openapi"].(string)
	if !strings.HasPrefix(v, "3.0.") && !strings.HasPrefix(v, "3.1.") {
		return nil, fmt.Errorf("OpenAPI version %q is not supported; use 3.0 or 3.1", v)
	}
	paths, ok := root["paths"].(map[string]any)
	if !ok {
		return nil, errors.New("no paths object")
	}
	r := &resolver{root: root, steps: maxSteps}
	rootServer, rootErr := r.server(root["servers"])
	a := &api{}
	for _, path := range sortedKeys(paths) {
		item := r.object(paths[path])
		if err := r.take(); err != nil {
			return nil, fmt.Errorf("path %q: %w", path, err)
		}
		if item == nil {
			continue
		}
		pathErr := checkPath(path)
		shared := r.params(item["parameters"])
		pathServer, pathServerErr := r.server(item["servers"])
		for _, e := range []error{r.take(), pathServerErr} {
			if pathErr == nil {
				pathErr = e
			}
		}
		for _, m := range methods {
			op := r.object(item[m])
			if op == nil && r.err == nil {
				continue
			}
			o := &operation{Method: strings.ToUpper(m), Path: path}
			o.ID, _ = op["operationId"].(string)
			o.Summary, _ = op["summary"].(string)
			o.Params = r.merge(shared, r.params(op["parameters"]))
			o.Body = r.body(op["requestBody"])
			opServer, opServerErr := r.server(op["servers"])
			err := pathErr
			for _, e := range []error{r.take(), opServerErr, checkTemplate(path, o.Params)} {
				if err == nil {
					err = e
				}
			}
			switch {
			case opServer != "":
				o.ServerURL = opServer
			case pathServer != "":
				o.ServerURL = pathServer
			default:
				o.ServerURL = rootServer
				if err == nil {
					err = rootErr
				}
			}
			if err != nil {
				o.Unsupported = err.Error()
			}
			a.Ops = append(a.Ops, o)
		}
	}
	a.name(r)
	if r.steps < 0 {
		return nil, errTooComplex
	}
	return a, nil
}

// checkPath accepts only plain absolute paths, so a path cannot add a query,
// a fragment or an authority to the request URL.
func checkPath(path string) error {
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") || strings.ContainsAny(path, "?#\\ ") || hasControl(path) {
		return fmt.Errorf("path %q is not a plain absolute path", path)
	}
	return nil
}

// checkTemplate makes sure the path variables and path parameters match.
func checkTemplate(path string, params []param) error {
	vars := map[string]bool{}
	rest := path
	for {
		i := strings.IndexAny(rest, "{}")
		if i < 0 {
			break
		}
		j := strings.IndexByte(rest[i:], '}')
		if rest[i] == '}' || j < 0 {
			return fmt.Errorf("path %q has unbalanced braces", path)
		}
		vars[rest[i+1:i+j]] = true
		rest = rest[i+j+1:]
	}
	for _, p := range params {
		if p.In == "path" {
			if !vars[p.Name] {
				return fmt.Errorf("path parameter %q is not in the path", p.Name)
			}
			delete(vars, p.Name)
		}
	}
	if left := sortedKeys(vars); len(left) > 0 {
		return fmt.Errorf("path variable {%s} has no path parameter", left[0])
	}
	return nil
}

// name gives every operation a unique command name. It is the kebab-case
// operationId, or method and path when there is none. A name that clashes
// with another operation's or with a built-in command gets the method
// appended, then a number.
func (a *api) name(r *resolver) {
	base := make(map[*operation]string, len(a.Ops))
	count := map[string]int{}
	for _, o := range a.Ops {
		b := commandName(o.ID)
		if b == "" {
			b = commandName(strings.ToLower(o.Method) + "-" + o.Path)
		}
		if b == "" {
			b = "operation"
		}
		base[o] = b
		count[b]++
	}
	r.spend(len(a.Ops) * 20) // sorting
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
	for _, b := range builtins {
		used[b] = true
	}
	for _, o := range a.Ops { // unique names first, so a suffixed name cannot take one
		if b := base[o]; count[b] == 1 && !used[b] {
			o.Name = b
			used[b] = true
		}
	}
	next := map[string]int{} // next number to try for each suffixed name
	for _, o := range a.Ops {
		if o.Name != "" {
			continue
		}
		r.spend(1)
		name := base[o] + "-" + strings.ToLower(o.Method)
		if used[name] {
			cand := name
			n := max(next[cand], 2)
			for used[fmt.Sprintf("%s-%d", cand, n)] && r.spend(1) {
				n++
			}
			name = fmt.Sprintf("%s-%d", cand, n)
			next[cand] = n + 1
		}
		o.Name = name
		used[name] = true
	}
	sort.Slice(a.Ops, func(i, j int) bool { return a.Ops[i].Name < a.Ops[j].Name })
	a.byName = make(map[string]*operation, len(a.Ops))
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

// server returns the first URL of a "servers" list with its variables set
// to their defaults, or "" for an absent or empty list.
func (r *resolver) server(v any) (string, error) {
	list, _ := v.([]any)
	if len(list) == 0 {
		return "", nil
	}
	s, _ := list[0].(map[string]any)
	u, ok := s["url"].(string)
	if !ok {
		return "", errors.New("a server has no url")
	}
	vars, _ := s["variables"].(map[string]any)
	for _, name := range sortedKeys(vars) {
		r.spend(1)
		if m, ok := vars[name].(map[string]any); ok {
			if def, ok := m["default"].(string); ok {
				u = strings.ReplaceAll(u, "{"+name+"}", def)
			}
		}
	}
	if strings.ContainsAny(u, "{}") || hasControl(u) {
		return "", fmt.Errorf("server URL %q has a variable without a default", u)
	}
	if _, err := url.Parse(u); err != nil {
		return "", fmt.Errorf("server URL %q is not a URL", u)
	}
	if u == "" {
		u = "/"
	}
	return u, nil
}

// merge lets operation parameters override path-level ones with the same
// name and location.
func (r *resolver) merge(shared, own []param) []param {
	r.spend(len(shared) + len(own))
	seen := make(map[[2]string]bool, len(own))
	out := make([]param, 0, len(own)+len(shared))
	for _, p := range own {
		key := [2]string{p.In, p.Name}
		if seen[key] {
			r.fail(fmt.Errorf("parameter %q in %s is declared twice", p.Name, p.In))
			continue
		}
		seen[key] = true
		out = append(out, p)
	}
	for _, s := range shared {
		if key := [2]string{s.In, s.Name}; !seen[key] {
			seen[key] = true
			out = append(out, s)
		}
	}
	return out
}

// resolver follows local references and records the first error since the
// last take.
type resolver struct {
	root  map[string]any
	steps int
	err   error
}

func (r *resolver) spend(n int) bool {
	r.steps -= n
	return r.steps >= 0
}

func (r *resolver) fail(err error) {
	if r.err == nil {
		r.err = err
	}
}

func (r *resolver) take() error {
	err := r.err
	r.err = nil
	return err
}

// object follows $ref chains and returns the JSON object they end at, or
// nil for an absent value. A reference that cannot be resolved is an error.
func (r *resolver) object(v any) map[string]any {
	for depth := 0; depth < 32; depth++ {
		if !r.spend(1) {
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
		target, err := r.pointer(ref)
		if err != nil {
			r.fail(err)
			return nil
		}
		v = target
	}
	r.fail(errors.New("reference chain is too long or circular"))
	return nil
}

// pointer resolves a local reference: a URI fragment holding a JSON
// Pointer (RFC 6901), percent-decoded, with ~1 for '/' and ~0 for '~'.
func (r *resolver) pointer(ref string) (any, error) {
	frag, ok := strings.CutPrefix(ref, "#")
	if !ok {
		return nil, fmt.Errorf("reference %q is not local; only #/... references are supported", ref)
	}
	p, err := url.PathUnescape(frag)
	if err != nil {
		return nil, fmt.Errorf("reference %q is not a valid URI fragment", ref)
	}
	if p == "" {
		return any(r.root), nil
	}
	if !strings.HasPrefix(p, "/") {
		return nil, fmt.Errorf("reference %q is not a JSON Pointer", ref)
	}
	var cur any = r.root
	for _, tok := range strings.Split(p[1:], "/") {
		if !r.spend(1) {
			return nil, errTooComplex
		}
		if strings.Contains(strings.ReplaceAll(strings.ReplaceAll(tok, "~0", ""), "~1", ""), "~") {
			return nil, fmt.Errorf("reference %q has an invalid ~ escape", ref)
		}
		tok = strings.ReplaceAll(strings.ReplaceAll(tok, "~1", "/"), "~0", "~")
		switch c := cur.(type) {
		case map[string]any:
			next, ok := c[tok]
			if !ok {
				return nil, fmt.Errorf("reference %q does not resolve", ref)
			}
			cur = next
		case []any:
			i, err := strconv.Atoi(tok)
			if err != nil || i < 0 || i >= len(c) || tok[0] == '+' || (len(tok) > 1 && tok[0] == '0') {
				return nil, fmt.Errorf("reference %q does not resolve", ref)
			}
			cur = c[i]
		default:
			return nil, fmt.Errorf("reference %q does not resolve", ref)
		}
	}
	return cur, nil
}

// styles lists the supported parameter styles per location; the first is
// the default.
var styles = map[string][]string{
	"path":   {"simple", "label", "matrix"},
	"query":  {"form", "spaceDelimited", "pipeDelimited"},
	"header": {"simple"},
	"cookie": {"form"},
}

func (r *resolver) params(v any) []param {
	var list []any
	switch t := v.(type) {
	case nil:
	case []any:
		list = t
	default:
		r.fail(errors.New("parameters is not a list"))
	}
	var out []param
	for _, item := range list {
		p := r.object(item)
		if p == nil {
			continue
		}
		name, _ := p["name"].(string)
		in, _ := p["in"].(string)
		if name == "" || in == "" {
			r.fail(errors.New("a parameter has no name or location"))
			continue
		}
		allowed, ok := styles[in]
		if !ok {
			r.fail(fmt.Errorf("parameter %q is in %q; supported locations are path, query, header and cookie", name, in))
			continue
		}
		if _, ok := p["content"]; ok {
			r.fail(fmt.Errorf("parameter %q uses content instead of schema", name))
		}
		if b, _ := p["allowReserved"].(bool); b {
			r.fail(fmt.Errorf("parameter %q uses allowReserved", name))
		}
		if (in == "header" || in == "cookie") && !validToken(name) {
			r.fail(fmt.Errorf("%s parameter %q has an invalid name", in, name))
		}
		desc, _ := p["description"].(string)
		req, _ := p["required"].(bool)
		style, _ := p["style"].(string)
		if style == "" {
			style = allowed[0]
		}
		if !contains(allowed, style) {
			r.fail(fmt.Errorf("%s parameter %q uses style %q; supported: %s", in, name, style, strings.Join(allowed, ", ")))
		}
		explode, ok := p["explode"].(bool)
		if !ok {
			explode = style == "form"
		}
		array, object := r.schemaKind(p["schema"])
		switch {
		case object:
			r.fail(fmt.Errorf("parameter %q is an object", name))
		case array && in == "cookie":
			r.fail(fmt.Errorf("cookie parameter %q is an array", name))
		case !array && (style == "spaceDelimited" || style == "pipeDelimited"):
			r.fail(fmt.Errorf("query parameter %q uses style %q but is not an array", name, style))
		}
		out = append(out, param{Name: name, In: in, Description: desc, Required: req || in == "path",
			Array: array, Style: style, Explode: explode})
	}
	return out
}

// schemaKind reports whether a parameter schema is an array or an object.
func (r *resolver) schemaKind(v any) (array, object bool) {
	s := r.object(v)
	is := func(want string) bool {
		switch t := s["type"].(type) {
		case string:
			return t == want
		case []any: // OpenAPI 3.1 type lists
			for _, x := range t {
				if x == want {
					return true
				}
			}
		}
		return false
	}
	return is("array"), is("object") || s["properties"] != nil
}

// jsonMediaType reports whether a media type is JSON: application/json or
// any type with a +json suffix, such as application/merge-patch+json.
func jsonMediaType(mt string) bool {
	mt = strings.ToLower(strings.TrimSpace(strings.SplitN(mt, ";", 2)[0]))
	return mt == "application/json" || strings.HasSuffix(mt, "+json")
}

// mediaRank orders the request media types the CLI can send: JSON first,
// then raw bytes. Zero means unsupported.
func mediaRank(mt string) int {
	switch {
	case strings.EqualFold(mt, "application/json"):
		return 3
	case jsonMediaType(mt):
		return 2
	case strings.EqualFold(mt, "application/octet-stream"):
		return 1
	}
	return 0
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
		if mediaRank(mt) > mediaRank(b.MediaType) {
			b.MediaType = mt
		}
	}
	if b.MediaType == "" {
		r.fail(fmt.Errorf("request body media types [%s] are not supported; supported: application/json, */*+json, application/octet-stream",
			strings.Join(b.Declared, ", ")))
		return b
	}
	b.JSON = jsonMediaType(b.MediaType)
	if !b.JSON {
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

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
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
