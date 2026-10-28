// Command mailglo calls the Mailglo API from a terminal, a script or an agent.
// It reads the list of operations from the server's OpenAPI description, so
// every API operation is available with the same permissions as the web app.
//
//	mailglo operations                         list operations
//	mailglo help <operation>                   show parameters and body fields
//	mailglo <operation> [--param value]... [--body JSON|@file|-]
//	mailglo version                            show the CLI version
//
// MAILGLO_URL (default https://mailglo.com) or the --server flag selects the
// server, and MAILGLO_TOKEN authenticates. Output is JSON. Exit codes:
// 0 success, 1 other error, 2 usage, 3 authentication or permission,
// 4 not found, 5 conflict or stale state, 6 rate limit or outage (retry later).
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
)

// Set at build time with -ldflags "-X main.version=... -X main.commit=... -X main.date=...".
var (
	version = "dev"
	commit  = ""
	date    = ""
)

const defaultServer = "https://mailglo.com"

const usage = `usage:
  mailglo [--server URL] operations
  mailglo [--server URL] help <operation>
  mailglo [--server URL] <operation> [--param value]... [--body JSON|@file|-]
  mailglo version

environment:
  MAILGLO_URL    server URL (default ` + defaultServer + `); --server overrides it
  MAILGLO_TOKEN  API token from your Mailglo workspace`

// Exit codes.
const (
	exitOK       = 0
	exitError    = 1
	exitUsage    = 2
	exitAuth     = 3
	exitNotFound = 4
	exitConflict = 5
	exitRetry    = 6
)

func main() { os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)) }

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	server := os.Getenv("MAILGLO_URL")
	for len(args) > 0 {
		a := args[0]
		if a == "--server" {
			if len(args) < 2 {
				return fail(stderr, exitUsage, "--server needs a URL")
			}
			server, args = args[1], args[2:]
			continue
		}
		if v, ok := strings.CutPrefix(a, "--server="); ok {
			server, args = v, args[1:]
			continue
		}
		break
	}
	if server == "" {
		server = defaultServer
	}
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, usage)
		return exitUsage
	}
	switch args[0] {
	case "version", "--version":
		v, c, d := buildInfo()
		return emit(stdout, map[string]string{"version": v, "commit": c, "date": d, "go": runtime.Version()})
	case "-h", "--help":
		_, _ = fmt.Fprintln(stdout, usage)
		return exitOK
	case "help":
		if len(args) == 1 {
			_, _ = fmt.Fprintln(stdout, usage)
			return exitOK
		}
	}

	base, err := parseServer(server)
	if err != nil {
		return fail(stderr, exitUsage, err.Error())
	}
	token := os.Getenv("MAILGLO_TOKEN")
	if token != "" && args[0] != "operations" && args[0] != "help" && !safeForToken(base) {
		return fail(stderr, exitUsage, fmt.Sprintf("refusing to send MAILGLO_TOKEN to %s: use an https URL (plain http is allowed only for localhost)", base.Redacted()))
	}
	raw, code := loadSpec(base, stderr)
	if code != exitOK {
		return code
	}
	ops, err := parseSpec(raw)
	if err != nil {
		return fail(stderr, exitError, "cannot read the API description: "+err.Error())
	}

	switch args[0] {
	case "operations":
		var list []map[string]string
		seen := map[*operation]bool{}
		for _, o := range ops {
			if !seen[o] {
				seen[o] = true
				list = append(list, map[string]string{"operation": kebab(o.ID), "method": o.Method, "path": o.Path, "summary": o.Summary})
			}
		}
		sort.Slice(list, func(i, j int) bool { return list[i]["operation"] < list[j]["operation"] })
		return emit(stdout, list)
	case "help":
		o := ops[strings.ToLower(args[1])]
		if o == nil {
			return fail(stderr, exitUsage, "unknown operation; run: mailglo operations")
		}
		params := []map[string]any{}
		for _, p := range o.Params {
			params = append(params, map[string]any{"flag": "--" + p.Name, "in": p.In, "required": p.Required, "description": p.Description})
		}
		info := map[string]any{"operation": kebab(o.ID), "method": o.Method, "path": o.Path, "summary": o.Summary, "parameters": params}
		if o.Body != nil {
			info["body_required"] = o.Body.Required
			info["body_fields"] = o.Body.Fields
		}
		return emit(stdout, info)
	}

	o := ops[strings.ToLower(args[0])]
	if o == nil {
		return fail(stderr, exitUsage, fmt.Sprintf("unknown operation %q; run: mailglo operations", args[0]))
	}
	return call(base, o, args[1:], token, stdin, stdout, stderr)
}

// buildInfo reports the version stamped at release, or the module version
// when the binary was built with go install.
func buildInfo() (v, c, d string) {
	v, c, d = version, commit, date
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	if v == "dev" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		v = bi.Main.Version
	}
	for _, s := range bi.Settings {
		switch {
		case s.Key == "vcs.revision" && c == "":
			c = s.Value
		case s.Key == "vcs.time" && d == "":
			d = s.Value
		}
	}
	return
}

func emit(w io.Writer, v any) int {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
	return exitOK
}

func fail(w io.Writer, code int, msg string) int {
	_, _ = fmt.Fprintln(w, "mailglo:", msg)
	return code
}

// kebab turns an operationId such as ListDomains into list-domains.
func kebab(s string) string {
	var b strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('-')
			}
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return b.String()
}
