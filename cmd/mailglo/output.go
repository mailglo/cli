package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// Text from the API description and from responses can contain terminal
// control sequences (for example OSC 52, which writes the clipboard). The
// CLI never prints C0 or C1 control characters or DEL from those sources:
// JSON output escapes them as \uXXXX and diagnostics as \xXX or \uXXXX.

func isControl(r rune) bool { return r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) }

// cleanText makes s safe to print as one line of a diagnostic.
func cleanText(s string) string { return clean(s, false) }

// cleanBody makes a non-JSON response body safe to print, keeping newlines
// and tabs.
func cleanBody(s string) string { return clean(s, true) }

func clean(s string, keepLines bool) string {
	var b strings.Builder
	for i, w := 0, 0; i < len(s); i += w {
		r, size := utf8.DecodeRuneInString(s[i:])
		w = size
		switch {
		case r == utf8.RuneError && size == 1:
			fmt.Fprintf(&b, `\x%02x`, s[i])
		case keepLines && (r == '\n' || r == '\t'):
			b.WriteRune(r)
		case r < 0x80 && isControl(r):
			fmt.Fprintf(&b, `\x%02x`, r)
		case isControl(r):
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// escapeJSON escapes DEL and C1 characters in JSON text, which encoding/json
// leaves as they are, and replaces invalid UTF-8. Outside strings JSON has
// only ASCII punctuation, so these occur only inside strings, where \uXXXX
// means the same character.
func escapeJSON(b []byte) []byte {
	if utf8.Valid(b) && !bytes.ContainsFunc(b, func(r rune) bool { return r == 0x7f || (r >= 0x80 && r <= 0x9f) }) {
		return b
	}
	var out bytes.Buffer
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		if r == utf8.RuneError && size == 1 {
			out.WriteString(`\ufffd`)
		} else if r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			fmt.Fprintf(&out, `\u%04x`, r)
		} else {
			out.Write(b[:size])
		}
		b = b[size:]
	}
	return out.Bytes()
}

// emit writes v as indented JSON. A failed write is an error exit.
func emit(w io.Writer, stderr io.Writer, v any) int {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return fail(stderr, exitError, "cannot encode output: "+err.Error())
	}
	return write(w, stderr, escapeJSON(buf.Bytes()))
}

// write writes b and reports a failed write as an error exit.
func write(w, stderr io.Writer, b []byte) int {
	if _, err := w.Write(b); err != nil {
		return fail(stderr, exitError, "cannot write output: "+err.Error())
	}
	return exitOK
}

// fail prints a diagnostic and returns code.
func fail(w io.Writer, code int, msg string) int {
	_, _ = fmt.Fprintln(w, "mailglo:", cleanText(msg))
	return code
}

// warn prints a diagnostic that does not end the command.
func warn(w io.Writer, msg string) {
	_, _ = fmt.Fprintln(w, "mailglo:", cleanText(msg))
}
