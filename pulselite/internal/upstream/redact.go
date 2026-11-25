package upstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"sort"
	"strings"
	"syscall"

	"github.com/ethereum/go-ethereum/rpc"
)

// Redact reduces an endpoint URL to scheme://host. It drops the path, query,
// fragment and userinfo, where providers put API keys. Use it for every log
// line and error that mentions an upstream.
func Redact(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "<redacted>"
	}
	return u.Scheme + "://" + u.Host
}

// RedactAll redacts a list of endpoint URLs and joins them with commas.
func RedactAll(urls []string) string {
	out := make([]string, len(urls))
	for i, u := range urls {
		out[i] = Redact(u)
	}
	return strings.Join(out, ",")
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}

// TransportError is an upstream failure with the endpoint URL removed. The
// standard library puts the full request URL into transport errors, so they
// never leave this package unwrapped.
type TransportError struct {
	Host   string
	Reason string
}

func (e *TransportError) Error() string { return "upstream " + e.Host + ": " + e.Reason }

func (e *Endpoint) sanitize(err error) error { return sanitize(e.URL, e.name, err) }

// sanitize makes err safe to show to RPC callers and logs. Transport errors
// become a TransportError with a short reason. JSON-RPC errors from the
// upstream keep their type, with the host prepended. Secret parts of raw in
// the message become "<redacted>". If any part of raw is still in the message
// or in the error data after that, in any form or letter case, the error
// fails closed to "upstream <host>: request failed" (a JSON-RPC error keeps
// its code and loses its data).
func sanitize(raw, host string, err error) error {
	if err == nil {
		return nil
	}
	var (
		ue  *url.Error
		ne  net.Error
		he  rpc.HTTPError
		hep *rpc.HTTPError
	)
	switch {
	case errors.As(err, &ue), errors.As(err, &ne), errors.As(err, &he), errors.As(err, &hep):
		return &TransportError{Host: host, Reason: classify(err)}
	case errors.Is(err, context.DeadlineExceeded):
		return &TransportError{Host: host, Reason: "timeout"}
	}
	text := err.Error()
	clean := scrub(raw, text)
	if stillLeaks(raw, clean) || dataLeaks(raw, err) {
		// Scrubbing is best-effort; what it missed must not get out.
		var re rpc.Error
		if errors.As(err, &re) {
			return &redactedRPCError{msg: "upstream " + host + ": request failed", code: re.ErrorCode()}
		}
		return &TransportError{Host: host, Reason: "request failed"}
	}
	if clean != text {
		msg := "upstream " + host + ": " + clean
		var re rpc.Error
		if errors.As(err, &re) {
			return &redactedRPCError{msg: msg, code: re.ErrorCode()}
		}
		return errors.New(msg)
	}
	return fmt.Errorf("upstream %s: %w", host, err)
}

// redactedRPCError is a JSON-RPC error from the upstream whose message had
// the endpoint's secrets replaced. It keeps the error code.
type redactedRPCError struct {
	msg  string
	code int
}

func (e *redactedRPCError) Error() string  { return e.msg }
func (e *redactedRPCError) ErrorCode() int { return e.code }

// classify gives a short reason for a transport error. It never includes the
// error text, which can contain the URL.
func classify(err error) string {
	var (
		ne  net.Error
		de  *net.DNSError
		he  rpc.HTTPError
		hep *rpc.HTTPError
	)
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.As(err, &he):
		return "http status " + he.Status
	case errors.As(err, &hep):
		return "http status " + hep.Status
	case errors.As(err, &de):
		return "dns lookup failed"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.Is(err, syscall.ECONNRESET):
		return "connection reset"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "connection closed"
	case errors.As(err, &ne) && ne.Timeout():
		return "timeout"
	}
	return "request failed"
}

// minSecretLen is the shortest path segment or query value treated as a
// secret. Shorter parts ("v2", "eth") are route names, not keys.
const minSecretLen = 6

const redactedMark = "<redacted>"

// secrets returns the parts of raw that can hold a key, longest first: the
// whole path and query, each path segment and query value, and the userinfo,
// each in escaped and decoded form.
func secrets(raw string) []string {
	u, err := url.Parse(raw)
	if err != nil {
		return []string{raw}
	}
	seen := map[string]bool{}
	var out []string
	add := func(s string, minLen int) {
		if len(s) < minLen || seen[s] || strings.Contains(redactedMark, s) {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	addBoth := func(s string, minLen int, unescape func(string) (string, error)) {
		add(s, minLen)
		if d, err := unescape(s); err == nil {
			add(d, minLen)
		}
	}
	if p := u.EscapedPath(); len(p) > 1 {
		add(p, 2)
		add(u.Path, 2)
		for _, seg := range strings.Split(p, "/") {
			addBoth(seg, minSecretLen, url.PathUnescape)
		}
	}
	if u.RawQuery != "" {
		add(u.RawQuery, 1)
		for _, kv := range strings.Split(u.RawQuery, "&") {
			_, v, _ := strings.Cut(kv, "=")
			addBoth(v, minSecretLen, url.QueryUnescape)
		}
	}
	if u.User != nil {
		add(u.User.Username(), 1)
		add(url.PathEscape(u.User.Username()), 1)
		if pw, ok := u.User.Password(); ok {
			add(pw, 1)
			add(url.PathEscape(pw), 1)
		}
	}
	sort.Slice(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}

// urlParts returns every part of raw that can hold a key, whatever its
// length: the userinfo, each path segment, and each query part with its name
// and value, in raw, decoded and re-encoded forms.
func urlParts(raw string) []string {
	u, err := url.Parse(raw)
	if err != nil {
		return []string{raw}
	}
	var base []string
	if u.User != nil {
		base = append(base, u.User.Username())
		if pw, ok := u.User.Password(); ok {
			base = append(base, pw)
		}
	}
	base = append(base, strings.Split(u.EscapedPath(), "/")...)
	base = append(base, strings.Split(u.Path, "/")...)
	for _, q := range []string{u.RawQuery, u.EscapedFragment()} {
		for _, part := range strings.FieldsFunc(q, func(r rune) bool { return r == '&' || r == ';' }) {
			name, value, _ := strings.Cut(part, "=")
			base = append(base, part, name, value)
		}
	}
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, s := range base {
		add(s)
		for _, unescape := range []func(string) (string, error){url.PathUnescape, url.QueryUnescape} {
			if d, err := unescape(s); err == nil {
				add(d)
				add(url.PathEscape(d))
				add(url.QueryEscape(d))
			}
		}
	}
	return out
}

// stillLeaks reports whether msg, ignoring case and the redaction marks,
// contains any part of raw that can hold a key.
func stillLeaks(raw, msg string) bool {
	m := strings.ToLower(strings.ReplaceAll(msg, redactedMark, " "))
	for _, p := range urlParts(raw) {
		if strings.Contains(m, strings.ToLower(p)) {
			return true
		}
	}
	return false
}

// dataLeaks reports whether the JSON-RPC error data of err names a part of
// raw. Error data is never scrubbed, only checked.
func dataLeaks(raw string, err error) bool {
	var de rpc.DataError
	if !errors.As(err, &de) || de.ErrorData() == nil {
		return false
	}
	return stillLeaks(raw, fmt.Sprint(de.ErrorData()))
}

// scrub replaces every secret part of raw in msg with "<redacted>".
func scrub(raw, msg string) string {
	for _, s := range secrets(raw) {
		msg = strings.ReplaceAll(msg, s, redactedMark)
	}
	return msg
}
