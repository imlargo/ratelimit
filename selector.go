package ratelimit

import (
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"
)

// ErrInvalidSelector is the class of all selector validation failures.
var ErrInvalidSelector = errors.New("invalid selector")

// knownMethods is the set a selector may name. net/http.ServeMux accepts any
// token here, so a typo like "GTE /api/" registers happily and then never
// matches anything - a rule that silently does nothing. We refuse it instead.
var knownMethods = map[string]bool{
	http.MethodGet: true, http.MethodHead: true, http.MethodPost: true,
	http.MethodPut: true, http.MethodPatch: true, http.MethodDelete: true,
	http.MethodConnect: true, http.MethodOptions: true, http.MethodTrace: true,
	// WebDAV and friends, since they show up in real APIs.
	"PROPFIND": true, "PROPPATCH": true, "MKCOL": true, "COPY": true,
	"MOVE": true, "LOCK": true, "UNLOCK": true, "REPORT": true, "SEARCH": true,
}

type segment struct {
	lit  string
	wild bool // {name}: matches exactly one non-empty segment
}

// selector decides whether a rule applies to a request.
//
// The syntax is net/http.ServeMux's, so there is nothing new to learn: an
// optional method, an optional host, a path with {single} and {rest...}
// wildcards and the {$} end anchor. Patterns are validated by registering them
// with a throwaway ServeMux, so the errors are the standard library's.
//
// The matching is ours, for two reasons that are not negotiable:
//
//   - A ServeMux reports only its single most specific match. Rules here are
//     additive: every rule whose selector matches is evaluated, so we need the
//     whole set. A ServeMux also panics when two registered patterns overlap
//     without one being more specific, which is exactly the shape of a global
//     rule plus a narrower one.
//   - ServeMux.Handler costs a couple of hundred nanoseconds and allocates.
//     Per rule, per request, that is the entire latency budget.
type selector struct {
	raw    string
	method string // "" means any; GET also matches HEAD
	host   string // "" means any
	segs   []segment
	prefix bool // pattern ended in "/" or in {rest...}
	anchor bool // {$}: the path must end here, with a trailing slash
	all    bool // matches every request
}

func compileSelector(raw string) (selector, error) {
	if raw == "" {
		return selector{raw: "", all: true}, nil
	}

	s := selector{raw: raw}
	rest := raw

	// Method.
	if i := strings.IndexAny(rest, " \t"); i >= 0 {
		m := rest[:i]
		if m == "" {
			return selector{}, fmt.Errorf("%q: %w: leading whitespace", raw, ErrInvalidSelector)
		}
		if !knownMethods[m] {
			if knownMethods[strings.ToUpper(m)] {
				return selector{}, fmt.Errorf("%q: %w: method %q must be upper case, did you mean %q?",
					raw, ErrInvalidSelector, m, strings.ToUpper(m))
			}
			return selector{}, fmt.Errorf("%q: %w: %q is not an HTTP method. "+
				"net/http.ServeMux would accept it and then never match anything; write a known method or omit it to match every method",
				raw, ErrInvalidSelector, m)
		}
		s.method = m
		rest = strings.TrimLeft(rest[i:], " \t")
	}

	// Host and path.
	slash := strings.IndexByte(rest, '/')
	if slash < 0 {
		return selector{}, fmt.Errorf("%q: %w: no path; a selector needs a path starting with %q", raw, ErrInvalidSelector, "/")
	}
	s.host = rest[:slash]
	p := rest[slash:]

	// Let the standard library be the judge of the pattern itself.
	if err := validateWithServeMux(s.method, s.host+p); err != nil {
		return selector{}, fmt.Errorf("%q: %w: %v", raw, ErrInvalidSelector, err)
	}

	// Segments.
	if strings.HasSuffix(p, "/") {
		s.prefix = true
		p = p[:len(p)-1]
	}
	for _, seg := range splitSegments(p) {
		switch {
		case seg == "{$}":
			s.anchor = true
			s.prefix = false
		case strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}"):
			name := seg[1 : len(seg)-1]
			if strings.HasSuffix(name, "...") {
				s.prefix = true
			} else {
				s.segs = append(s.segs, segment{wild: true})
			}
		default:
			s.segs = append(s.segs, segment{lit: seg})
		}
	}
	if len(s.segs) == 0 && s.prefix && s.host == "" && s.method == "" {
		s.all = true
	}
	return s, nil
}

func splitSegments(p string) []string {
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

// validateWithServeMux borrows the standard library's pattern parser, including
// its error messages, without inheriting its matching.
func validateWithServeMux(method, hostPath string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	pat := hostPath
	if method != "" {
		pat = method + " " + hostPath
	}
	http.NewServeMux().Handle(pat, http.NotFoundHandler())
	return nil
}

// matches reports whether the selector applies. It reads the pre-normalised
// path from the request context of the caller and allocates nothing.
func (s *selector) matches(method, host, cleanPath string, endsWithSlash bool) bool {
	if s.all {
		return true
	}
	if s.method != "" && s.method != method {
		// A GET pattern also serves HEAD, matching ServeMux.
		if !(s.method == http.MethodGet && method == http.MethodHead) {
			return false
		}
	}
	if s.host != "" && s.host != stripPort(host) {
		return false
	}

	it := segIter{p: cleanPath}
	for i := range s.segs {
		seg, ok := it.next()
		if !ok {
			return false
		}
		if s.segs[i].wild {
			if seg == "" {
				return false // wildcards need a non-empty segment
			}
			continue
		}
		if seg != s.segs[i].lit {
			return false
		}
	}
	_, more := it.next()
	switch {
	case s.prefix:
		// A prefix selector claims everything at or below its path. This is
		// deliberately more inclusive than ServeMux, which would redirect
		// "/api" to "/api/" rather than match it: a security control that lets
		// a request through because of a missing trailing slash is a hole.
		return true
	case s.anchor:
		return !more && endsWithSlash
	default:
		return !more && !endsWithSlash
	}
}

type segIter struct {
	p string
	i int
}

func (it *segIter) next() (string, bool) {
	if it.i >= len(it.p) {
		return "", false
	}
	if it.p[it.i] == '/' {
		it.i++
	}
	if it.i > len(it.p) {
		return "", false
	}
	start := it.i
	for it.i < len(it.p) && it.p[it.i] != '/' {
		it.i++
	}
	if start == it.i && start >= len(it.p) {
		return "", false
	}
	return it.p[start:it.i], true
}

func stripPort(host string) string {
	if len(host) == 0 {
		return host
	}
	// A bracketed IPv6 literal, with or without a port.
	if host[0] == '[' {
		if j := strings.IndexByte(host, ']'); j > 0 {
			return host[1:j]
		}
		return host
	}
	i := strings.IndexByte(host, ':')
	if i < 0 {
		return host
	}
	// More than one colon and no brackets means a bare IPv6 literal, not a port.
	if strings.IndexByte(host[i+1:], ':') >= 0 {
		return host
	}
	return host[:i]
}

// normalisePath returns a path safe to match selectors against, plus whether it
// ended in a slash.
//
// Matching a raw path lets "/api/../admin" and "//api/x" slip past a selector
// that names "/admin" or "/api/". The clean path is the one the router will
// resolve to, so it is the one a rule has to be written against. The common
// case - an already clean path - allocates nothing.
func normalisePath(p string) (string, bool) {
	if p == "" {
		return "/", true
	}
	endsSlash := p[len(p)-1] == '/'
	if isCleanPath(p) {
		return p, endsSlash
	}
	c := path.Clean(p)
	if endsSlash && c != "/" {
		c += "/"
	}
	if c == "" || c[0] != '/' {
		c = "/" + strings.TrimPrefix(c, "/")
	}
	return c, endsSlash
}

func isCleanPath(p string) bool {
	if p[0] != '/' {
		return false
	}
	for i := 0; i < len(p); i++ {
		if p[i] != '/' {
			continue
		}
		if i+1 < len(p) && p[i+1] == '/' {
			return false
		}
		// "/." must be "/.." or a segment starting with "." only if longer.
		if i+1 < len(p) && p[i+1] == '.' {
			switch {
			case i+2 == len(p): // trailing "/."
				return false
			case p[i+2] == '/': // "/./"
				return false
			case p[i+2] == '.' && (i+3 == len(p) || p[i+3] == '/'): // "/.." or "/../"
				return false
			}
		}
	}
	return true
}
