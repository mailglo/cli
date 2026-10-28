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
			`{"swagger":"2.0","paths":{}}`: `OpenAPI version "" is not supported`,
			`{"openapi":"2.0","paths":{}}`: `OpenAPI version "2.0" is not supported`,
			`{"openapi":"3.0.3"}`:          "no paths object",
			`{"openapi":"3.0.3","paths":{"/x":{"$ref":"#/components/nothing"}}}`: `reference "#/components/nothing" does not resolve`,
		} {
			r := newEnv(t, specServer(t, spec).URL).run(t, "operations")
			want(t, r, 6)
			if !strings.Contains(r.Stderr, msg) {
				t.Fatalf("%s: stderr %s", spec, r.Stderr)
			}
		}
	})

	t.Run("operations outside the subset are listed and refused", func(t *testing.T) {
		ops := map[string]struct{ op, why string }{
			"/a":       {`{"operationId":"A","parameters":[{"name":"q","in":"query","content":{"application/json":{}}}]}`, `parameter "q" uses content instead of schema`},
			"/b":       {`{"operationId":"B","parameters":[{"name":"q","in":"query","allowReserved":true}]}`, `parameter "q" uses allowReserved`},
			"/c":       {`{"operationId":"C","parameters":[{"name":"q","in":"query","schema":{"type":"object"}}]}`, `parameter "q" is an object`},
			"/d":       {`{"operationId":"D","parameters":[{"name":"q","in":"query","style":"deepObject"}]}`, `uses style "deepObject"`},
			"/e":       {`{"operationId":"E","parameters":[{"name":"h","in":"header","style":"form"}]}`, `header parameter "h" uses style "form"`},
			"/f":       {`{"operationId":"F","parameters":[{"name":"c","in":"cookie","schema":{"type":"array"}}]}`, `cookie parameter "c" is an array`},
			"/g/{id}":  {`{"operationId":"G"}`, "path variable {id} has no path parameter"},
			"/h":       {`{"operationId":"H","parameters":[{"name":"id","in":"path","required":true}]}`, `path parameter "id" is not in the path`},
			"i":        {`{"operationId":"I"}`, `path "i" is not a plain absolute path`},
			"/j?x=1":   {`{"operationId":"J"}`, "is not a plain absolute path"},
			"/k":       {`{"operationId":"K","servers":[{"url":"/{v}"}]}`, "has a variable without a default"},
			"/l":       {`{"operationId":"L","parameters":[{"name":"q","in":"body"}]}`, `parameter "q" is in "body"`},
			"/m":       {`{"operationId":"M","requestBody":{"content":{"multipart/form-data":{}}}}`, "request body media types [multipart/form-data] are not supported"},
			"/n":       {`{"operationId":"N","parameters":[{"$ref":"other.json#/p"}]}`, `reference "other.json#/p" is not local`},
			"/o":       {`{"operationId":"O","parameters":[{"name":"q","in":"query"},{"name":"q","in":"query"}]}`, `parameter "q" in query is declared twice`},
			"/p":       {`{"operationId":"P","parameters":[{"name":"q","in":"query","style":"pipeDelimited"}]}`, "is not an array"},
			"/ok/{id}": {`{"operationId":"Ok","parameters":[{"name":"id","in":"path","required":true}]}`, ""},
			"/q":       {`{"operationId":"Q","parameters":[{"$ref":"#/components/parameters/Missing"}]}`, `reference "#/components/parameters/Missing" does not resolve`},
			"/r":       {`{"operationId":"R","requestBody":{"$ref":"#/components/requestBodies/Gone"}}`, "does not resolve"},
			"/s":       {`{"operationId":"S","parameters":[{"name":"q","in":"query","schema":{"$ref":"#/components/schemas/A"}}]}`, "too long or circular"},
			"/t":       {`{"operationId":"T","parameters":[{"name":"bad name","in":"header"}]}`, `header parameter "bad name" has an invalid name`},
			"/u":       {`{"operationId":"U","parameters":[{"name":"q","in":"query","schema":{"$ref":"#/components/schemas/A~2"}}]}`, "invalid ~ escape"},
		}
		var paths []string
		for p, o := range ops {
			paths = append(paths, fmt.Sprintf("%q:{\"get\":%s}", p, o.op))
		}
		spec := `{"openapi":"3.0.3","components":{"schemas":{"A":{"$ref":"#/components/schemas/B"},"B":{"$ref":"#/components/schemas/A"}}},"paths":{` + strings.Join(paths, ",") + `}}`
		srv := specServer(t, spec)
		e := newEnv(t, srv.URL)
		r := e.run(t, "operations")
		want(t, r, 0)
		listed := map[string]string{}
		for _, o := range decode[[]map[string]string](t, r.Stdout) {
			listed[o["path"]] = o["unsupported"]
		}
		for p, o := range ops {
			if (o.why == "") != (listed[p] == "") || !strings.Contains(listed[p], o.why) {
				t.Errorf("%s: unsupported %q, want %q", p, listed[p], o.why)
			}
		}
		r = e.run(t, "a", "--q", "1")
		want(t, r, 2)
		if !strings.Contains(r.Stderr, "this CLI cannot call a: parameter") {
			t.Fatalf("stderr %s", r.Stderr)
		}
		if h := decode[map[string]any](t, e.run(t, "help", "c").Stdout); !strings.Contains(fmt.Sprint(h["unsupported"]), "is an object") {
			t.Fatalf("help %v", h)
		}
		if srv.callCount() != 0 {
			t.Fatal("an unsupported operation reached the server")
		}
		want(t, e.run(t, "ok", "--id", "1"), 0)
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
	spec := fmt.Sprintf(`{"openapi":"3.0.3","servers":[{"url":"/"}],"paths":{
		"x-@%s/leak":{"get":{"operationId":"Leak"}},
		"/me":{"get":{"operationId":"GetMe","parameters":[{"name":"session","in":"cookie","required":true}]}}}}`, host)
	srv := specServer(t, spec, func(f *fakeServer) {
		f.respond = func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, other.URL+"/me", http.StatusFound)
		}
	})
	e := newEnv(t, srv.URL)
	e.vars["MAILGLO_TOKEN"] = testToken
	r := e.run(t, "leak")
	want(t, r, 2)
	if !strings.Contains(r.Stderr, "not a plain absolute path") {
		t.Fatalf("stderr %s", r.Stderr)
	}
	r = newEnv(t, srv.URL).run(t, "get-me", "--session", "secret")
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
	for i := 0; i < 8000; i++ {
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
	for i := 0; i < 20000; i++ {
		shared = append(shared, map[string]any{"name": fmt.Sprintf("s%d", i), "in": "query"})
		own = append(own, map[string]any{"name": fmt.Sprintf("o%d", i), "in": "query"})
	}
	paths["/wide"] = map[string]any{"parameters": shared, "get": map[string]any{"operationId": "Wide", "parameters": own}}
	raw, _ := json.Marshal(map[string]any{"openapi": "3.0.3", "paths": paths})

	start := time.Now()
	a, err := parseSpec(raw)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if limit := time.Second; !raceEnabled && elapsed > limit {
		t.Fatalf("parsing took %s", elapsed)
	}
	if len(a.Ops) != 8001 || a.byName["x-get"] == nil || a.byName["x-get-8000"] == nil || a.byName["x-get-8001"] != nil {
		t.Fatalf("names: %d operations", len(a.Ops))
	}
	if w := a.byName["wide"]; w == nil || len(w.Params) != 40000 {
		t.Fatal("parameters were not merged")
	}

	r := newEnv(t, specServer(t, string(raw)).URL).run(t, "help", "x-get-4000")
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
			"requestBodies":{"MyBody":{"required":true,"content":{"application/json":{"schema":{"$ref":"#/components/schemas/a~1b~0c"}}}}},
			"schemas":{"a/b~c":{"required":["name"],"properties":{"name":{}}}}},
		"paths":{
			"/source":{"get":{"operationId":"Source","parameters":[{"name":"must","in":"query","required":true}]}},
			"/copy":{"get":{"operationId":"Copy","parameters":[{"$ref":"#/paths/~1source/get/parameters/0"}]},
				"post":{"operationId":"Make","requestBody":{"$ref":"#/components/requestBodies/My%42ody"}}},
			"/bad":{"get":{"operationId":"Bad","parameters":[{"$ref":"#/paths/~1source/get/parameters/01"}]}}}}`
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
	want(t, e.run(t, "make"), 2) // the referenced body is required
	r := e.run(t, "bad")
	want(t, r, 2)
	if !strings.Contains(r.Stderr, "does not resolve") {
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
