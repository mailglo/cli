package main

import (
	"bytes"
	"encoding/json"
	"errors"
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
func emit(w io.Writer, stderr io.Writer, v any) int { return emitLimited(w, stderr, v, "the output") }

// emitLimited encodes v as indented JSON into memory, one value at a time,
// and stops as soon as the output passes maxOutputBytes. Only a complete
// result within the limit is written; otherwise nothing reaches w and the
// command exits 1.
func emitLimited(w io.Writer, stderr io.Writer, v any, what string) int {
	lw := &limitWriter{max: maxOutputBytes}
	err := encodeIndented(lw, v, 0)
	if err == nil {
		_, err = lw.Write([]byte("\n"))
	}
	if errors.Is(err, errOutputLimit) {
		return fail(stderr, exitError, fmt.Sprintf("%s would be larger than %d MiB; the API description is too large to print", what, maxOutputBytes>>20))
	}
	if err != nil {
		return fail(stderr, exitError, "cannot encode output: "+err.Error())
	}
	return write(w, stderr, lw.buf.Bytes())
}

// encodeIndented writes v as json.Encoder with SetIndent("", "  ") and
// SetEscapeHTML(false) would, followed by escapeJSON, but container by
// container, so the limit applies while encoding rather than after.
func encodeIndented(w io.Writer, v any, depth int) error {
	switch v := v.(type) {
	case map[string]any:
		return encodeObject(w, len(v), sortedKeys(v), func(k string) any { return v[k] }, depth)
	case map[string]string:
		return encodeObject(w, len(v), sortedKeys(v), func(k string) any { return v[k] }, depth)
	case []map[string]string:
		return encodeArray(w, len(v), v == nil, func(i int) any { return v[i] }, depth)
	case []map[string]any:
		return encodeArray(w, len(v), v == nil, func(i int) any { return v[i] }, depth)
	case []string:
		return encodeArray(w, len(v), v == nil, func(i int) any { return v[i] }, depth)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return err
	}
	_, err := w.Write(escapeJSON(bytes.TrimSuffix(buf.Bytes(), []byte("\n"))))
	return err
}

func encodeObject(w io.Writer, n int, keys []string, get func(string) any, depth int) error {
	if n == 0 {
		_, err := io.WriteString(w, "{}")
		return err
	}
	if _, err := io.WriteString(w, "{\n"); err != nil {
		return err
	}
	for i, k := range keys {
		if _, err := io.WriteString(w, strings.Repeat("  ", depth+1)); err != nil {
			return err
		}
		if err := encodeIndented(w, k, depth+1); err != nil {
			return err
		}
		if _, err := io.WriteString(w, ": "); err != nil {
			return err
		}
		if err := encodeIndented(w, get(k), depth+1); err != nil {
			return err
		}
		sep := ",\n"
		if i == len(keys)-1 {
			sep = "\n"
		}
		if _, err := io.WriteString(w, sep); err != nil {
			return err
		}
	}
	_, err := io.WriteString(w, strings.Repeat("  ", depth)+"}")
	return err
}

func encodeArray(w io.Writer, n int, isNil bool, get func(int) any, depth int) error {
	switch {
	case isNil:
		_, err := io.WriteString(w, "null")
		return err
	case n == 0:
		_, err := io.WriteString(w, "[]")
		return err
	}
	if _, err := io.WriteString(w, "[\n"); err != nil {
		return err
	}
	for i := 0; i < n; i++ {
		if _, err := io.WriteString(w, strings.Repeat("  ", depth+1)); err != nil {
			return err
		}
		if err := encodeIndented(w, get(i), depth+1); err != nil {
			return err
		}
		sep := ",\n"
		if i == n-1 {
			sep = "\n"
		}
		if _, err := io.WriteString(w, sep); err != nil {
			return err
		}
	}
	_, err := io.WriteString(w, strings.Repeat("  ", depth)+"]")
	return err
}

var errOutputLimit = errors.New("output limit reached")

// limitWriter buffers up to max bytes and fails the write that would pass it.
type limitWriter struct {
	buf bytes.Buffer
	max int
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if l.buf.Len()+len(p) > l.max {
		return 0, errOutputLimit
	}
	return l.buf.Write(p)
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
