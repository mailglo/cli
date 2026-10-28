package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
)

// This file turns an OpenAPI description (JSON) into the operations the CLI
// can call. kin-openapi loads and validates the document and resolves its
// references; this file then applies the subset documented under
// "Supported API descriptions" in README.md. A document that does not load
// or validate is refused. An operation outside the subset is kept, marked
// unsupported with the construct that put it there, and refused when
// someone runs it.

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

	byName map[string][]int  // parameter indexes by name
	byLoc  map[[2]string]int // parameter index by location and name
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

// index builds the flag lookup tables once, so resolving a flag costs the
// same whatever the number of parameters.
func (o *operation) index() {
	o.byName = make(map[string][]int, len(o.Params))
	o.byLoc = make(map[[2]string]int, len(o.Params))
	for i, p := range o.Params {
		o.byName[p.Name] = append(o.byName[p.Name], i)
		o.byLoc[[2]string{p.In, p.Name}] = i
	}
}

// builtins are CLI commands; no operation is named after one.
var builtins = []string{"help", "operations", "version"}

const (
	// maxSpecBytes caps the description before it is parsed.
	maxSpecBytes = 16 << 20
	// parseTimeout bounds loading and validating it.
	parseTimeout = 20 * time.Second
	// maxSteps bounds the work after loading: parameters, body fields and
	// names.
	maxSteps = 4_000_000
	// maxNameBytes caps a generated command name.
	maxNameBytes = 200
)

var errTooComplex = errors.New("description is too complex")

// parseSpec reads an OpenAPI description. The result does not depend on map
// iteration order.
func parseSpec(raw []byte) (*api, error) {
	return parseSpecWithin(raw, parseTimeout)
}

func parseSpecWithin(raw []byte, timeout time.Duration) (*api, error) {
	if len(raw) > maxSpecBytes {
		return nil, fmt.Errorf("description is larger than %d bytes", maxSpecBytes)
	}
	var head struct {
		OpenAPI string `json:"openapi"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return nil, err
	}
	if !strings.HasPrefix(head.OpenAPI, "3.0.") && !strings.HasPrefix(head.OpenAPI, "3.1.") {
		return nil, fmt.Errorf("OpenAPI version %q is not supported; use 3.0 or 3.1", short(head.OpenAPI))
	}
	doc, err := load(raw, timeout)
	if err != nil {
		return nil, err
	}
	b := &budget{steps: maxSteps, fields: map[*openapi3.Schema]fieldList{}}
	a := &api{}
	rootServer, rootErr := server(doc.Servers)
	paths := doc.Paths.Map()
	for _, path := range sortedKeys(paths) {
		item := paths[path]
		if item == nil {
			continue
		}
		shared, sharedErr := b.params(item.Parameters)
		pathServer, pathServerErr := server(item.Servers)
		pathErr := firstErr(checkPath(path), sharedErr, pathServerErr)
		ops := item.Operations()
		for _, m := range sortedKeys(ops) {
			op := ops[m]
			o := &operation{Method: m, Path: path, ID: op.OperationID, Summary: op.Summary}
			own, ownErr := b.params(op.Parameters)
			o.Params = b.merge(shared, own)
			o.index()
			var bodyErr error
			o.Body, bodyErr = b.body(op.RequestBody)
			var opServer string
			var opServerErr error
			if op.Servers != nil {
				opServer, opServerErr = server(*op.Servers)
			}
			e := firstErr(pathErr, ownErr, bodyErr, opServerErr, checkTemplate(path, o.Params))
			switch {
			case opServer != "":
				o.ServerURL = opServer
			case pathServer != "":
				o.ServerURL = pathServer
			default:
				o.ServerURL = rootServer
				e = firstErr(e, rootErr)
			}
			if e != nil {
				o.Unsupported = e.Error()
			}
			a.Ops = append(a.Ops, o)
			if !b.spend(1) {
				return nil, errTooComplex
			}
		}
	}
	a.name(b)
	if b.steps < 0 {
		return nil, errTooComplex
	}
	return a, nil
}

// load runs kin-openapi's loader and validator under a deadline. Only
// references inside the document are followed.
func load(raw []byte, timeout time.Duration) (*openapi3.T, error) {
	type result struct {
		doc *openapi3.T
		err error
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	done := make(chan result, 1)
	go func() {
		// A malformed document can make the loader panic; that is an
		// invalid description like any other, not a crash.
		defer func() {
			if p := recover(); p != nil {
				done <- result{nil, fmt.Errorf("the loader failed on it (%v)", p)}
			}
		}()
		loader := openapi3.NewLoader()
		loader.IsExternalRefsAllowed = false
		loader.Context = ctx
		doc, err := loader.LoadFromData(raw)
		if err == nil {
			err = doc.Validate(ctx)
		}
		done <- result{doc, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			return nil, fmt.Errorf("invalid OpenAPI description: %s", shortN(r.err.Error(), 300))
		}
		if r.doc.Paths == nil {
			return nil, errors.New("no paths object")
		}
		return r.doc, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("%w: reading it took longer than %s", errTooComplex, timeout)
	}
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

// budget bounds the work after loading and remembers schema field lists.
type budget struct {
	steps  int
	fields map[*openapi3.Schema]fieldList
}

func (b *budget) spend(n int) bool {
	b.steps -= n
	return b.steps >= 0
}

// checkPath accepts only plain absolute paths, so a path cannot add a query,
// a fragment or an authority to the request URL.
func checkPath(path string) error {
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") || strings.ContainsAny(path, "?#\\ ") || hasControl(path) {
		return fmt.Errorf("path %q is not a plain absolute path", short(path))
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
			return fmt.Errorf("path %q has unbalanced braces", short(path))
		}
		vars[rest[i+1:i+j]] = true
		rest = rest[i+j+1:]
	}
	for _, p := range params {
		if p.In == "path" {
			if !vars[p.Name] {
				return fmt.Errorf("path parameter %q is not in the path", short(p.Name))
			}
			delete(vars, p.Name)
		}
	}
	if left := sortedKeys(vars); len(left) > 0 {
		return fmt.Errorf("path variable {%s} has no path parameter", short(left[0]))
	}
	return nil
}

// name gives every operation a unique command name. It is the kebab-case
// operationId, or method and path when there is none, at most maxNameBytes
// long. A name that clashes with another operation's or with a built-in
// command gets the method appended, then a number.
func (a *api) name(b *budget) {
	base := make(map[*operation]string, len(a.Ops))
	count := map[string]int{}
	for _, o := range a.Ops {
		n := commandName(o.ID)
		if n == "" {
			n = commandName(strings.ToLower(o.Method) + "-" + o.Path)
		}
		if n == "" {
			n = "operation"
		}
		b.spend(len(n))
		base[o] = n
		count[n]++
	}
	b.spend(len(a.Ops) * 20) // sorting
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
	for _, n := range builtins {
		used[n] = true
	}
	for _, o := range a.Ops { // unique names first, so a suffixed name cannot take one
		if n := base[o]; count[n] == 1 && !used[n] {
			o.Name = n
			used[n] = true
		}
	}
	next := map[string]int{} // next number to try for each suffixed name
	for _, o := range a.Ops {
		if o.Name != "" {
			continue
		}
		name := base[o] + "-" + strings.ToLower(o.Method)
		b.spend(len(name))
		if used[name] {
			cand := name
			n := max(next[cand], 2)
			for used[fmt.Sprintf("%s-%d", cand, n)] && b.spend(1) {
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
		return nil, fmt.Errorf("%q matches several operations: %s", short(s), strings.Join(names, ", "))
	}
	return nil, fmt.Errorf("unknown operation %q; run: mailglo operations", short(s))
}

// commandName turns an operationId, or "method-/path/{param}", into a
// lower-case name made of letters, digits, '-', '_' and '.', at most
// maxNameBytes long. Only the start of a long input is read.
func commandName(s string) string {
	if len(s) > 2*maxNameBytes {
		s = s[:2*maxNameBytes]
	}
	var b strings.Builder
	dash := false
	for _, r := range kebab(s) {
		if b.Len() >= maxNameBytes {
			break
		}
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

// server returns the first server URL with its variables set to their
// defaults, or "" when there is none.
func server(list openapi3.Servers) (string, error) {
	if len(list) == 0 || list[0] == nil {
		return "", nil
	}
	vars := map[string]string{}
	for name, v := range list[0].Variables {
		if v != nil {
			vars[name] = v.Default
		}
	}
	u, err := expandServer(list[0].URL, vars)
	if err != nil {
		return "", err
	}
	if hasControl(u) {
		return "", fmt.Errorf("server URL %q has a control character", short(u))
	}
	if _, err := url.Parse(u); err != nil {
		return "", fmt.Errorf("server URL %q is not a URL", short(u))
	}
	if u == "" {
		u = "/"
	}
	return u, nil
}

// maxServerURL caps a server URL after variable substitution.
const maxServerURL = 2048

// expandServer replaces each {name} in a server URL with the variable's
// default, in one pass: a default is never expanded again.
func expandServer(u string, vars map[string]string) (string, error) {
	var b strings.Builder
	rest := u
	for {
		i := strings.IndexAny(rest, "{}")
		if i < 0 {
			b.WriteString(rest)
			break
		}
		j := strings.IndexByte(rest[i:], '}')
		if rest[i] == '}' || j < 0 {
			return "", fmt.Errorf("server URL %q has unbalanced braces", short(u))
		}
		name := rest[i+1 : i+j]
		def, ok := vars[name]
		if !ok {
			return "", fmt.Errorf("server URL %q has a variable without a default", short(u))
		}
		if strings.ContainsAny(def, "{}") {
			return "", fmt.Errorf("server variable %q has braces in its default", short(name))
		}
		b.WriteString(rest[:i])
		b.WriteString(def)
		if b.Len() > maxServerURL {
			return "", fmt.Errorf("server URL %q is longer than %d bytes after substitution", short(u), maxServerURL)
		}
		rest = rest[i+j+1:]
	}
	if b.Len() > maxServerURL {
		return "", fmt.Errorf("server URL is longer than %d bytes", maxServerURL)
	}
	return b.String(), nil
}

// short trims text from the description for a diagnostic.
func short(s string) string { return shortN(s, 80) }

func shortN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}

// paramKey identifies a parameter for duplicates and overrides. Header
// names are compared the way HTTP sends them.
func paramKey(in, name string) [2]string {
	if in == "header" {
		name = http.CanonicalHeaderKey(name)
	}
	return [2]string{in, name}
}

// merge lets operation parameters override path-level ones with the same
// location and name.
func (b *budget) merge(shared, own []param) []param {
	b.spend(len(shared) + len(own))
	seen := make(map[[2]string]bool, len(own))
	out := make([]param, 0, len(own)+len(shared))
	for _, p := range own {
		seen[paramKey(p.In, p.Name)] = true
		out = append(out, p)
	}
	for _, s := range shared {
		if k := paramKey(s.In, s.Name); !seen[k] {
			seen[k] = true
			out = append(out, s)
		}
	}
	return out
}

// reservedHeaders are set by the CLI or by HTTP itself; a header parameter
// with one of these names could not be sent as given.
var reservedHeaders = map[string]bool{
	"Accept": true, "Accept-Encoding": true, "Authorization": true, "Connection": true, "Content-Length": true,
	"Content-Type": true, "Cookie": true, "Expect": true, "Host": true, "Keep-Alive": true, "Proxy-Authorization": true,
	"Proxy-Connection": true, "Te": true, "Trailer": true, "Transfer-Encoding": true, "Upgrade": true, "User-Agent": true,
}

// styles lists the supported parameter styles per location; the first is
// the default.
var styles = map[string][]string{
	"path":   {"simple", "label", "matrix"},
	"query":  {"form", "spaceDelimited", "pipeDelimited"},
	"header": {"simple"},
	"cookie": {"form"},
}

// params converts a parameter list, returning the first construct outside
// the subset.
func (b *budget) params(list openapi3.Parameters) ([]param, error) {
	b.spend(len(list))
	var out []param
	var first error
	fail := func(err error) {
		if first == nil {
			first = err
		}
	}
	seen := map[[2]string]bool{}
	for _, ref := range list {
		if ref == nil || ref.Value == nil {
			fail(errors.New("a parameter does not resolve"))
			continue
		}
		p := ref.Value
		name, in := p.Name, p.In
		allowed, ok := styles[in]
		if !ok {
			fail(fmt.Errorf("parameter %q is in %q; supported locations are path, query, header and cookie", short(name), short(in)))
			continue
		}
		k := paramKey(in, name)
		if seen[k] {
			fail(fmt.Errorf("parameter %q in %s is declared twice", short(name), in))
			continue
		}
		seen[k] = true
		switch {
		case strings.Contains(name, "="):
			fail(fmt.Errorf("parameter %q has '=' in its name, which a flag cannot carry", short(name)))
		case in == "header" && reservedHeaders[http.CanonicalHeaderKey(name)]:
			fail(fmt.Errorf("header parameter %q is reserved: the CLI or HTTP sets it", short(name)))
		case (in == "header" || in == "cookie") && !validToken(name):
			fail(fmt.Errorf("%s parameter %q has an invalid name", in, short(name)))
		case p.Content != nil:
			fail(fmt.Errorf("parameter %q uses content instead of schema", short(name)))
		case p.AllowReserved:
			fail(fmt.Errorf("parameter %q uses allowReserved", short(name)))
		}
		sm, err := p.SerializationMethod()
		if err != nil {
			fail(err)
			continue
		}
		if !contains(allowed, sm.Style) {
			fail(fmt.Errorf("%s parameter %q uses style %q; supported: %s", in, short(name), short(sm.Style), strings.Join(allowed, ", ")))
		}
		array, err := paramShape(name, p.Schema)
		if err != nil {
			fail(err)
		}
		switch {
		case array && in == "cookie":
			fail(fmt.Errorf("cookie parameter %q is an array", short(name)))
		case !array && (sm.Style == "spaceDelimited" || sm.Style == "pipeDelimited"):
			fail(fmt.Errorf("query parameter %q uses style %q but is not an array", short(name), sm.Style))
		}
		out = append(out, param{Name: name, In: in, Description: p.Description, Required: p.Required || in == "path",
			Array: array, Style: sm.Style, Explode: sm.Explode})
	}
	return out, first
}

// paramShape accepts a scalar schema, or an array of scalars.
func paramShape(name string, ref *openapi3.SchemaRef) (array bool, err error) {
	if ref == nil || ref.Value == nil {
		return false, nil
	}
	s := ref.Value
	if err := scalar(name, s, true); err != nil {
		return false, err
	}
	if !s.Type.Includes("array") {
		return false, nil
	}
	if s.Items == nil || s.Items.Value == nil {
		return true, nil
	}
	return true, scalar(name+" items", s.Items.Value, false)
}

// scalar refuses composed, conditional, object and (unless allowArray)
// array schemas.
func scalar(name string, s *openapi3.Schema, allowArray bool) error {
	switch {
	case len(s.AllOf) > 0:
		return fmt.Errorf("parameter %q has a schema using allOf", short(name))
	case len(s.OneOf) > 0:
		return fmt.Errorf("parameter %q has a schema using oneOf", short(name))
	case len(s.AnyOf) > 0:
		return fmt.Errorf("parameter %q has a schema using anyOf", short(name))
	case s.Not != nil:
		return fmt.Errorf("parameter %q has a schema using not", short(name))
	case s.If != nil || len(s.PrefixItems) > 0 || len(s.DependentSchemas) > 0:
		return fmt.Errorf("parameter %q has a conditional or tuple schema", short(name))
	case s.Type.Includes("object") || len(s.Properties) > 0 || s.AdditionalProperties.Has != nil || s.AdditionalProperties.Schema != nil:
		return fmt.Errorf("parameter %q is an object", short(name))
	case !allowArray && s.Type.Includes("array"):
		return fmt.Errorf("parameter %q is an array of arrays", short(name))
	}
	return nil
}

// mediaKind classifies a declared media type: 3 application/json, 2 another
// JSON type (+json), 1 application/octet-stream, 0 unsupported.
func mediaKind(mt string) int {
	t, _, err := mime.ParseMediaType(mt)
	if err != nil {
		return 0
	}
	typ, sub, ok := strings.Cut(t, "/")
	switch {
	case !ok || typ == "" || sub == "" || strings.Contains(sub, "/"):
		return 0
	case t == "application/json":
		return 3
	case strings.HasSuffix(sub, "+json") && len(sub) > len("+json"):
		return 2
	case t == "application/octet-stream":
		return 1
	}
	return 0
}

// jsonMediaType reports whether a media type is a JSON type.
func jsonMediaType(mt string) bool { return mediaKind(mt) >= 2 }

func (b *budget) body(ref *openapi3.RequestBodyRef) (*body, error) {
	if ref == nil {
		return nil, nil
	}
	if ref.Value == nil {
		return nil, errors.New("the request body does not resolve")
	}
	rb := ref.Value
	out := &body{Required: rb.Required, Declared: sortedKeys(rb.Content)}
	b.spend(len(out.Declared))
	for _, mt := range out.Declared {
		if mediaKind(mt) > mediaKind(out.MediaType) {
			out.MediaType = mt
		}
	}
	if mediaKind(out.MediaType) == 0 {
		out.MediaType = ""
		var names []string
		for _, mt := range out.Declared {
			names = append(names, short(mt))
		}
		return out, fmt.Errorf("request body media types [%s] are not supported; supported: application/json, */*+json, application/octet-stream",
			strings.Join(names, ", "))
	}
	out.JSON = jsonMediaType(out.MediaType)
	if mt := rb.Content[out.MediaType]; out.JSON && mt != nil && mt.Schema != nil && mt.Schema.Value != nil {
		f := b.bodyFields(mt.Schema.Value)
		out.Fields, out.RequiredFields = f.fields, f.required
	}
	return out, nil
}

type fieldList struct{ fields, required []string }

// bodyFields lists a body schema's top-level properties, including those of
// its allOf members. Each schema is visited once, and the result is shared
// by every operation that uses the schema.
func (b *budget) bodyFields(s *openapi3.Schema) fieldList {
	if f, ok := b.fields[s]; ok {
		return f
	}
	names, required := map[string]bool{}, map[string]bool{}
	seen := map[*openapi3.Schema]bool{}
	var walk func(*openapi3.Schema)
	walk = func(s *openapi3.Schema) {
		if s == nil || seen[s] || !b.spend(1+len(s.Properties)+len(s.Required)+len(s.AllOf)) {
			return
		}
		seen[s] = true
		for k := range s.Properties {
			names[k] = true
		}
		for _, k := range s.Required {
			required[k] = true
		}
		for _, sub := range s.AllOf {
			if sub != nil {
				walk(sub.Value)
			}
		}
	}
	walk(s)
	f := fieldList{sortedKeys(names), sortedKeys(required)}
	b.fields[s] = f
	return f
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
