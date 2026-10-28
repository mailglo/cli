package main

// End-to-end tests for the supported description subset, server URLs,
// references, naming limits, flag aliases and stderr failures.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestSupportedSubset(t *testing.T) {
	t.Run("documents outside the subset are refused", func(t *testing.T) {
		for spec, msg := range map[string]string{
			`{"swagger":"2.0","paths":{}}`:                                       `OpenAPI version "" is not supported`,
			`{"openapi":"2.0","paths":{}}`:                                       `OpenAPI version "2.0" is not supported`,
			`{"openapi":"3.0.3","info":{"title":"t","version":"1"}}`:             "paths",
			`{"openapi":"3.0.3","paths":{"/x":{"$ref":"#/components/nothing"}}}`: "#/components/nothing",
		} {
			r := newEnv(t, specServer(t, spec).URL).run(t, "operations")
			want(t, r, 6)
			if !strings.Contains(r.Stderr, msg) {
				t.Fatalf("%s: stderr %s", spec, r.Stderr)
			}
		}
	})

	// Each case is a path item next to a supported operation. "op" cases
	// mark only that operation unsupported; "doc" cases fail validation, so
	// the whole description is refused. Either way the message names the
	// construct.
	cases := []struct {
		path, item, level, why string
	}{
		{"/a", `{"get":{"operationId":"A","parameters":[{"name":"q","in":"query","content":{"application/json":{}}}]}}`, "op", `parameter "q" uses content instead of schema`},
		{"/b", `{"get":{"operationId":"B","parameters":[{"name":"q","in":"query","allowReserved":true}]}}`, "op", `parameter "q" uses allowReserved`},
		{"/c", `{"get":{"operationId":"C","parameters":[{"name":"q","in":"query","schema":{"type":"object"}}]}}`, "op", `parameter "q" is an object`},
		{"/d", `{"get":{"operationId":"D","parameters":[{"name":"q","in":"query","style":"deepObject","schema":{"type":"string"}}]}}`, "op", `uses style "deepObject"`},
		{"/e", `{"get":{"operationId":"E","parameters":[{"name":"h","in":"header","style":"form"}]}}`, "doc", `serialization method with style="form"`},
		{"/f", `{"get":{"operationId":"F","parameters":[{"name":"c","in":"cookie","schema":{"type":"array","items":{"type":"string"}}}]}}`, "op", `cookie parameter "c" is an array`},
		{"/g/{id}", `{"get":{"operationId":"G"}}`, "doc", "must define exactly all path parameters"},
		{"/h", `{"get":{"operationId":"H","parameters":[{"name":"id","in":"path","required":true}]}}`, "doc", "must define exactly all path parameters"},
		{"i", `{"get":{"operationId":"I"}}`, "doc", "does not start with a forward slash"},
		{"/j?x=1", `{"get":{"operationId":"J"}}`, "op", "is not a plain absolute path"},
		{"/k", `{"get":{"operationId":"K","servers":[{"url":"/{v}"}]}}`, "op", "has a variable without a default"},
		{"/l", `{"get":{"operationId":"L","parameters":[{"name":"q","in":"body"}]}}`, "doc", `'in' value "body"`},
		{"/m", `{"get":{"operationId":"M","requestBody":{"content":{"multipart/form-data":{}}}}}`, "op", "request body media types [multipart/form-data] are not supported"},
		{"/n", `{"get":{"operationId":"N","parameters":[{"$ref":"other.json#/p"}]}}`, "doc", "other.json"},
		{"/o", `{"get":{"operationId":"O","parameters":[{"name":"q","in":"query"},{"name":"q","in":"query"}]}}`, "doc", `more than one "query" parameter has name "q"`},
		{"/p", `{"get":{"operationId":"P","parameters":[{"name":"q","in":"query","style":"pipeDelimited","schema":{"type":"string"}}]}}`, "op", "is not an array"},
		{"/q", `{"get":{"operationId":"Q","parameters":[{"$ref":"#/components/parameters/Missing"}]}}`, "doc", "Missing"},
		{"/r", `{"get":{"operationId":"R","requestBody":{"$ref":"#/components/requestBodies/Gone"}}}`, "doc", "Gone"},
		{"/t", `{"get":{"operationId":"T","parameters":[{"name":"bad name","in":"header"}]}}`, "op", `header parameter "bad name" has an invalid name`},
		{"/u", `{"get":{"operationId":"U","parameters":[{"name":"q","in":"query","schema":{"$ref":"#/components/schemas/A~2"}}]}}`, "doc", "A~2"},
		{"/v1", `{"get":{"operationId":"V1","parameters":[{"name":"q","in":"query","schema":{"allOf":[{"type":"string"}]}}]}}`, "op", `parameter "q" has a schema using allOf`},
		{"/v2", `{"get":{"operationId":"V2","parameters":[{"name":"q","in":"query","schema":{"oneOf":[{"type":"string"},{"type":"integer"}]}}]}}`, "op", "using oneOf"},
		{"/v3", `{"get":{"operationId":"V3","parameters":[{"name":"q","in":"query","schema":{"$ref":"#/components/schemas/Any"}}]}}`, "op", "using anyOf"},
		{"/v4", `{"get":{"operationId":"V4","parameters":[{"name":"q","in":"query","schema":{"type":"array","items":{"type":"object"}}}]}}`, "op", `parameter "q items" is an object`},
		{"/v5", `{"get":{"operationId":"V5","parameters":[{"name":"q","in":"query","schema":{"type":"array","items":{"$ref":"#/components/schemas/Obj"}}}]}}`, "op", `parameter "q items" is an object`},
		{"/v6", `{"get":{"operationId":"V6","parameters":[{"name":"q","in":"query","schema":{"type":"array","items":{"type":"array","items":{"type":"string"}}}}]}}`, "op", "is an array of arrays"},
		{"/w1", `{"get":{"operationId":"W1","parameters":[{"name":"q","in":"query","schema":{"$ref":"#/components/schemas/Str","type":"array"}}]}}`, "doc", "type"},
		{"/w2", `{"get":{"operationId":"W2","requestBody":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/Str","required":["x"]}}}}}}`, "doc", "required"},
		{"/w3", `{"get":{"operationId":"W3","parameters":[{"$ref":"#/components/parameters/P","description":"Overrides the text."}]}}`, "doc", "extra sibling fields: [description]"},
		{"/x1/{id}", `{"parameters":[{"name":"id","in":"path","required":true},{"name":"id","in":"path","required":true}],"get":{"operationId":"X1"}}`, "doc", `more than one "path" parameter has name "id"`},
		{"/x2", `{"get":{"operationId":"X2","parameters":[{"name":"X-Name","in":"header"},{"name":"x-name","in":"header"}]}}`, "op", `parameter "x-name" in header is declared twice`},
		{"/y1", `{"get":{"operationId":"Y1","parameters":[{"name":"Host","in":"header"}]}}`, "op", `header parameter "Host" is reserved`},
		{"/y2", `{"get":{"operationId":"Y2","parameters":[{"name":"user-agent","in":"header"}]}}`, "op", `header parameter "user-agent" is reserved`},
		{"/y3", `{"get":{"operationId":"Y3","parameters":[{"name":"Cookie","in":"header"}]}}`, "op", "is reserved"},
		{"/y4", `{"get":{"operationId":"Y4","parameters":[{"name":"X-Trace","in":"header"}]}}`, "", ""},
		{"/z1", `{"get":{"operationId":"Z1","parameters":[{"name":"a=b","in":"query"}]}}`, "op", `parameter "a=b" has '=' in its name`},
		{"/z2", `{"get":{"operationId":"Z2","parameters":[{"$ref":"#/paths/~1ok~1%7Bid%7D/get/parameters/-0"}]}}`, "doc", `"-0"`},
		{"/z7", `{"get":{"operationId":"Z7","parameters":[{"$ref":"#/paths/~1ok~1%7Bid%7D/get/parameters/01"}]}}`, "doc", `"01"`},
		{"/z3", `{"get":{"operationId":"Z3","servers":[{"url":"{a}","variables":{"a":{"default":"{a}{a}"}}}]}}`, "op", `server variable "a" has braces in its default`},
		{"/z4", `{"get":{"operationId":"Z4","servers":{"url":"/v2"}}}`, "doc", "cannot unmarshal object into field Operation.servers"},
		{"/z5", `{"post":{"operationId":"Z5","requestBody":{"content":{"bogus+json":{}}}}}`, "op", "[bogus+json] are not supported"},
		{"/z6", `{"post":{"operationId":"Z6","requestBody":{"$ref":"#/components/schemas/Num"}}}`, "doc", "expecting ref to requestBody object"},
	}
	components := `"components":{"parameters":{"P":{"name":"p","in":"query","schema":{"type":"string"}}},
		"schemas":{"Str":{"type":"string"},"Num":{"type":"integer"},"Obj":{"type":"object"},"Any":{"anyOf":[{"type":"string"},{"type":"integer"}]}}}`
	ok := `"/ok/{id}":{"get":{"operationId":"Ok","parameters":[{"name":"id","in":"path","required":true}]}}`
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			spec := fmt.Sprintf(`{"openapi":"3.0.3",%s,"paths":{%q:%s,%s}}`, components, c.path, c.item, ok)
			srv := specServer(t, spec)
			e := newEnv(t, srv.URL)
			r := e.run(t, "operations")
			if c.level == "doc" {
				want(t, r, 6)
				if !strings.Contains(r.Stderr, "invalid OpenAPI description") || !strings.Contains(r.Stderr, c.why) {
					t.Fatalf("stderr %s, want it to name %s", r.Stderr, c.why)
				}
				return
			}
			want(t, r, 0)
			var got string
			for _, o := range decode[[]map[string]string](t, r.Stdout) {
				if o["path"] == c.path {
					got = o["unsupported"]
				}
			}
			if (c.why == "") != (got == "") || !strings.Contains(got, c.why) {
				t.Fatalf("unsupported %q, want %q", got, c.why)
			}
			want(t, e.run(t, "ok", "--id", "1"), 0)
		})
	}

	t.Run("an unsupported operation is refused", func(t *testing.T) {
		spec := `{"openapi":"3.0.3","paths":{"/a":{"get":{"operationId":"A","parameters":[{"name":"q","in":"query","schema":{"type":"object"}}]}}}}`
		srv := specServer(t, spec)
		e := newEnv(t, srv.URL)
		r := e.run(t, "a", "--q", "1")
		want(t, r, 2)
		if !strings.Contains(r.Stderr, `this CLI cannot call a: parameter "q" is an object`) {
			t.Fatalf("stderr %s", r.Stderr)
		}
		if h := decode[map[string]any](t, e.run(t, "help", "a").Stdout); !strings.Contains(fmt.Sprint(h["unsupported"]), "is an object") {
			t.Fatalf("help %v", h)
		}
		if srv.callCount() != 0 {
			t.Fatal("an unsupported operation reached the server")
		}
	})

	t.Run("raw request bodies", func(t *testing.T) {
		spec := `{"openapi":"3.0.3","paths":{"/upload":{"post":{"operationId":"Upload","requestBody":{"required":true,
			"content":{"application/octet-stream":{"schema":{"type":"string","format":"binary"}}}}}}}}`
		srv := specServer(t, spec)
		f := filepath.Join(t.TempDir(), "report.xml.gz")
		raw := []byte{0x1f, 0x8b, 0x08, 0x00, 'n', 'o', 't', ' ', 'j', 's', 'o', 'n'}
		if err := os.WriteFile(f, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		want(t, newEnv(t, srv.URL).run(t, "upload", "--body", "@"+f), 0)
		if c := srv.lastCall(t); c.ContentType != "application/octet-stream" || string(c.Body) != string(raw) {
			t.Fatalf("call %+v", c)
		}
	})
}

// Item 1 (round 2): no API request follows a redirect off the origin, and a
// description cannot point a request at another authority.
func TestRequestsStayOnOrigin(t *testing.T) {
	other := newFakeServer(t, false)
	host := strings.TrimPrefix(other.URL, "http://")
	// A path that would put another host in the request URL.
	for _, path := range []string{"@" + host + "/leak", "/x/../@" + host + "/leak"} {
		spec := fmt.Sprintf(`{"openapi":"3.0.3","servers":[{"url":"/"}],"paths":{%q:{"get":{"operationId":"Leak"}}}}`, path)
		e := newEnv(t, specServer(t, spec).URL)
		e.vars["MAILGLO_TOKEN"] = testToken
		_ = e.run(t, "leak")
		if other.callCount() != 0 {
			t.Fatalf("%s: the other server was called", path)
		}
	}
	spec := `{"openapi":"3.0.3","paths":{"/me":{"get":{"operationId":"GetMe","parameters":[{"name":"session","in":"cookie","required":true}]}}}}`
	srv := specServer(t, spec, func(f *fakeServer) {
		f.respond = func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, other.URL+"/me", http.StatusFound)
		}
	})
	r := newEnv(t, srv.URL).run(t, "get-me", "--session", "secret")
	want(t, r, 6)
	if other.callCount() != 0 {
		t.Fatal("the session cookie followed a redirect to another origin")
	}
}

// Item 2 (round 2): a link inside the cache directory is refused even when
// it points at a file in the same directory.
func TestCacheRefusesInnerLinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic links need privileges on Windows")
	}
	srv := newFakeServer(t, false)
	e := newEnv(t, srv.URL)
	want(t, e.run(t, "operations"), 0)
	path := cacheFileIn(t, e.cache, srv.URL)
	if err := os.Rename(path, path+".real"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Base(path)+".real", path); err != nil {
		t.Fatal(err)
	}
	srv.set(func() { srv.specStatus = http.StatusInternalServerError })
	r := e.run(t, "operations")
	want(t, r, 6)
	if !strings.Contains(r.Stderr, "not a regular file") {
		t.Fatalf("stderr %s", r.Stderr)
	}
}

// Item 3 (round 2): naming and parameter merging stay linear.
func TestAdversarialNamingIsFast(t *testing.T) {
	paths := map[string]any{}
	n, m := size(8000), size(20000)
	for i := 0; i < n; i++ {
		// Every id reduces to the same command name "x".
		suffix := strings.Map(func(r rune) rune {
			if r == '0' {
				return '!'
			}
			return '?'
		}, fmt.Sprintf("%020b", i))
		paths[fmt.Sprintf("/p%d", i)] = map[string]any{"get": map[string]any{"operationId": "X" + suffix}}
	}
	var shared, own []any
	for i := 0; i < m; i++ {
		shared = append(shared, map[string]any{"name": fmt.Sprintf("s%d", i), "in": "query"})
		own = append(own, map[string]any{"name": fmt.Sprintf("o%d", i), "in": "query"})
	}
	paths["/wide"] = map[string]any{"parameters": shared, "get": map[string]any{"operationId": "Wide", "parameters": own}}
	raw, _ := json.Marshal(map[string]any{"openapi": "3.0.3", "paths": paths})
	raw = completeSpec(raw)

	// Loading and validating is kin-openapi's work; what this CLI adds on
	// top (parameters, merging, names) must stay well under a second.
	start := time.Now()
	if _, err := load(raw, parseTimeout); err != nil {
		t.Fatal(err)
	}
	loading := time.Since(start)
	start = time.Now()
	a, err := parseSpec(raw)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if limit := time.Second; !raceEnabled && elapsed-loading > limit {
		t.Fatalf("parsing took %s, of which loading %s", elapsed, loading)
	}
	if len(a.Ops) != n+1 || a.byName["x-get"] == nil || a.byName[fmt.Sprintf("x-get-%d", n)] == nil || a.byName[fmt.Sprintf("x-get-%d", n+1)] != nil {
		t.Fatalf("names: %d operations", len(a.Ops))
	}
	if w := a.byName["wide"]; w == nil || len(w.Params) != 2*m {
		t.Fatal("parameters were not merged")
	}

	r := newEnv(t, specServer(t, string(raw)).URL).run(t, "help", fmt.Sprintf("x-get-%d", n/2))
	want(t, r, 0)
}

// Items 4 and 5 (round 2): path- and operation-level servers, resolved
// against the description's final URL.
func TestServerOverrides(t *testing.T) {
	spec := `{"openapi":"3.0.3","servers":[{"url":"/root"}],"paths":{
		"/things":{"servers":[{"url":"/path"}],
			"get":{"operationId":"Fetch","servers":[{"url":"/op"}]},
			"post":{"operationId":"Add"}},
		"/other":{"get":{"operationId":"Other"}},
		"/far":{"get":{"operationId":"Far","servers":[{"url":"https://elsewhere.example/far-base"}]}}}}`
	srv := specServer(t, spec)
	e := newEnv(t, srv.URL)
	e.vars["MAILGLO_TOKEN"] = testToken
	for cmd, path := range map[string]string{"fetch": "/op/things", "add": "/path/things", "other": "/root/other", "far": "/far-base/far"} {
		want(t, e.run(t, cmd), 0)
		if c := srv.lastCall(t); c.Path != path || c.Auth != "Bearer "+testToken {
			t.Fatalf("%s: %s, want %s", cmd, c.Path, path)
		}
	}

	t.Run("relative to the final description URL", func(t *testing.T) {
		srv := specServer(t, `{"openapi":"3.0.3","servers":[{"url":"."}],"paths":{"/things":{"get":{"operationId":"Fetch"}}}}`,
			func(f *fakeServer) { f.specRedirect = "/v2/openapi.json" })
		e := newEnv(t, srv.URL)
		for i := 0; i < 2; i++ { // fresh, then revalidated with 304
			want(t, e.run(t, "fetch"), 0)
			if c := srv.lastCall(t); c.Path != "/v2/things" {
				t.Fatalf("run %d: %s", i, c.Path)
			}
		}
		// From the cache when the description cannot be fetched.
		srv.set(func() { srv.specStatus = http.StatusServiceUnavailable })
		want(t, e.run(t, "fetch"), 0)
		if c := srv.lastCall(t); c.Path != "/v2/things" {
			t.Fatalf("from cache: %s", c.Path)
		}
	})
}

// Item 6 (round 2): label and matrix path styles.
func TestPathStyles(t *testing.T) {
	cases := []struct {
		style   string
		explode bool
		array   bool
		want    string
	}{
		{"simple", false, true, "/items/a,b"},
		{"simple", true, true, "/items/a,b"},
		{"label", false, true, "/items/.a,b"},
		{"label", true, true, "/items/.a.b"},
		{"matrix", false, true, "/items/;id=a,b"},
		{"matrix", true, true, "/items/;id=a;id=b"},
		{"label", false, false, "/items/.a"},
		{"matrix", false, false, "/items/;id=a"},
	}
	for _, c := range cases {
		schema := `{"type":"string"}`
		args := []string{"fetch", "--id", "a"}
		if c.array {
			schema = `{"type":"array","items":{"type":"string"}}`
			args = append(args, "--id", "b")
		}
		spec := fmt.Sprintf(`{"openapi":"3.0.3","paths":{"/items/{id}":{"get":{"operationId":"Fetch","parameters":[
			{"name":"id","in":"path","required":true,"style":%q,"explode":%v,"schema":%s}]}}}}`, c.style, c.explode, schema)
		srv := specServer(t, spec)
		want(t, newEnv(t, srv.URL).run(t, args...), 0)
		if got := srv.lastCall(t).Path; got != "/api/v1"+c.want {
			t.Errorf("%s explode=%v array=%v: %s, want %s", c.style, c.explode, c.array, got, c.want)
		}
	}
}

// Item 7 (round 2): built-in command names are reserved.
func TestBuiltinNamesAreReserved(t *testing.T) {
	srv := specServer(t, `{"openapi":"3.0.3","paths":{"/x":{"get":{"operationId":"version"}},"/y":{"get":{"operationId":"operations"}},"/z":{"get":{"operationId":"help"}}}}`)
	e := newEnv(t, srv.URL)
	r := e.run(t, "operations")
	want(t, r, 0)
	var names []string
	for _, o := range decode[[]map[string]string](t, r.Stdout) {
		names = append(names, o["operation"])
	}
	if strings.Join(names, " ") != "help-get operations-get version-get" {
		t.Fatalf("names %v", names)
	}
	r = e.run(t, "version")
	want(t, r, 0)
	if _, ok := decode[map[string]string](t, r.Stdout)["go"]; !ok || srv.callCount() != 0 {
		t.Fatal("version did not print the CLI version")
	}
	for cmd, path := range map[string]string{"version-get": "/api/v1/x", "operations-get": "/api/v1/y", "help-get": "/api/v1/z"} {
		want(t, e.run(t, cmd), 0)
		if c := srv.lastCall(t); c.Path != path {
			t.Fatalf("%s: %s", cmd, c.Path)
		}
	}
}

// Item 8 (round 2): references are JSON Pointers in URI fragments.
func TestJSONPointerReferences(t *testing.T) {
	spec := `{"openapi":"3.0.3",
		"components":{
			"requestBodies":{"MyBody":{"required":true,"content":{"application/json":{"schema":{"$ref":"#/components/schemas/Named"}}}}},
			"schemas":{"Named":{"required":["name"],"properties":{"name":{}}}}},
		"paths":{
			"/source":{"get":{"operationId":"Source","parameters":[{"name":"must","in":"query","required":true}]}},
			"/t~x":{"get":{"operationId":"Tilde","parameters":[{"name":"also","in":"query","required":true}]}},
			"/other":{"get":{"operationId":"Other","parameters":[{"$ref":"#/paths/~1t~0x/get/parameters/0"}]}},
			"/copy":{"get":{"operationId":"Copy","parameters":[{"$ref":"#/paths/~1source/get/parameters/0"}]},
				"post":{"operationId":"Make","requestBody":{"$ref":"#/components/requestBodies/My%42ody"}}}}}`
	srv := specServer(t, spec)
	e := newEnv(t, srv.URL)
	want(t, e.run(t, "copy"), 2) // the referenced required parameter is enforced
	want(t, e.run(t, "copy", "--must", "1"), 0)
	if c := srv.lastCall(t); c.RawQuery != "must=1" {
		t.Fatalf("query %s", c.RawQuery)
	}
	info := decode[map[string]any](t, e.run(t, "help", "make").Stdout)
	if fmt.Sprint(info["body_fields"]) != "[name]" || info["body_needed"] != true {
		t.Fatalf("help %v", info)
	}
	want(t, e.run(t, "make"), 2)  // the referenced body is required
	want(t, e.run(t, "other"), 2) // ~0 and ~1 in a pointer
	want(t, e.run(t, "other", "--also", "1"), 0)
	bad := `{"openapi":"3.0.3","paths":{"/source":{"get":{"operationId":"Source","parameters":[{"name":"must","in":"query"}]}},
		"/bad":{"get":{"operationId":"Bad","parameters":[{"$ref":"#/paths/~1source/get/parameters/01"}]}}}}`
	r := newEnv(t, specServer(t, bad).URL).run(t, "bad")
	want(t, r, 6)
	if !strings.Contains(r.Stderr, `"01"`) {
		t.Fatalf("stderr %s", r.Stderr)
	}
}

// Item 9 (round 2): a literal name that looks qualified gets an
// unambiguous flag.
func TestDottedParameterNames(t *testing.T) {
	srv := specServer(t, `{"openapi":"3.0.3","paths":{"/things/{id}":{"get":{"operationId":"Fetch","parameters":[
		{"name":"id","in":"path","required":true},{"name":"path.id","in":"query","required":true}]}}}}`)
	e := newEnv(t, srv.URL)
	info := decode[map[string]any](t, e.run(t, "help", "fetch").Stdout)
	var flags []string
	for _, p := range info["parameters"].([]any) {
		flags = append(flags, p.(map[string]any)["flag"].(string))
	}
	if strings.Join(flags, " ") != "--id --query.path.id" {
		t.Fatalf("flags %v", flags)
	}
	want(t, e.run(t, "fetch", flags[0], "a", flags[1], "b"), 0)
	if c := srv.lastCall(t); c.Path != "/api/v1/things/a" || c.RawQuery != "path.id=b" {
		t.Fatalf("call %+v", c)
	}
	// --path.id is the path parameter, so giving it with --id is a repeat.
	want(t, e.run(t, "fetch", "--id", "a", "--path.id", "b"), 2)
}

// Item 10 (round 2): a warning that cannot be written fails the command.
func TestWarningWriteFailureExitsNonzero(t *testing.T) {
	full, err := os.OpenFile("/dev/full", os.O_WRONLY, 0)
	if err != nil {
		t.Skip("no /dev/full")
	}
	defer func() { _ = full.Close() }()
	srv := newFakeServer(t, false, func(f *fakeServer) {
		f.respond = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Result-Complete", "false")
			w.Header().Set("X-Next-Cursor", "next")
			_, _ = w.Write([]byte(`[]`))
		}
	})
	e := newEnv(t, srv.URL)
	e.stderr = full
	if r := e.run(t, "list-domains"); r.Code != 1 {
		t.Fatalf("partial list with unwritable stderr: exit %d", r.Code)
	}
	// The stale-cache warning as well.
	srv.set(func() { srv.specStatus = http.StatusServiceUnavailable })
	if r := e.run(t, "operations"); r.Code != 1 {
		t.Fatalf("stale cache with unwritable stderr: exit %d", r.Code)
	}
}
