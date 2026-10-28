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
	"fmt"
	"io"
	"os"
	"runtime"
	"runtime/debug"
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

flags:
  --name value, --name=value   a parameter; repeat it for a list
  --path.name, --query.name    a parameter by location, when names clash
  --header.name, --cookie.name
  --body, --request-body       the JSON request body: inline, @file, or - for stdin

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

// run runs the CLI. Diagnostics carry results too (a partial list, a stale
// cache), so a failed write to stderr makes a successful command exit 1.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	errOut := &trackedWriter{w: stderr}
	code := runCommand(args, stdin, stdout, errOut)
	if errOut.failed && code == exitOK {
		code = exitError
	}
	return code
}

type trackedWriter struct {
	w      io.Writer
	failed bool
}

func (t *trackedWriter) Write(b []byte) (int, error) {
	n, err := t.w.Write(b)
	if err != nil {
		t.failed = true
	}
	return n, err
}

func runCommand(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
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
		return emit(stdout, stderr, map[string]string{"version": v, "commit": c, "date": d, "go": runtime.Version()})
	case "-h", "--help":
		return write(stdout, stderr, []byte(usage+"\n"))
	case "help":
		if len(args) == 1 {
			return write(stdout, stderr, []byte(usage+"\n"))
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
	a, docURL, code := loadSpec(base, stderr)
	if code != exitOK {
		return code
	}

	switch args[0] {
	case "operations":
		list := []map[string]string{}
		for _, o := range a.Ops {
			m := map[string]string{"operation": o.Name, "operation_id": o.ID, "method": o.Method, "path": o.Path, "summary": o.Summary}
			if o.Unsupported != "" {
				m["unsupported"] = o.Unsupported
			}
			list = append(list, m)
		}
		return emit(stdout, stderr, list)
	case "help":
		o, err := a.lookup(args[1])
		if err != nil {
			return fail(stderr, exitUsage, err.Error())
		}
		return emit(stdout, stderr, helpInfo(o))
	}

	o, err := a.lookup(args[0])
	if err != nil {
		return fail(stderr, exitUsage, err.Error())
	}
	return call(base, docURL, o, args[1:], token, stdin, stdout, stderr)
}

func helpInfo(o *operation) map[string]any {
	params := []map[string]any{}
	for i, p := range o.Params {
		m := map[string]any{"flag": paramFlag(o, i), "qualified_flag": "--" + p.In + "." + p.Name, "in": p.In,
			"required": p.Required, "description": p.Description}
		if p.Array {
			m["repeatable"] = true
		}
		params = append(params, m)
	}
	info := map[string]any{"operation": o.Name, "operation_id": o.ID, "method": o.Method, "path": o.Path,
		"summary": o.Summary, "parameters": params}
	if o.Unsupported != "" {
		info["unsupported"] = o.Unsupported
	}
	if b := o.Body; b != nil {
		info["body_flag"] = bodyFlag(o)
		info["body_needed"] = b.Required
		info["body_media_type"] = b.MediaType
		if b.JSON {
			info["body_fields"] = b.Fields
			info["body_required"] = b.RequiredFields
		}
	}
	return info
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
