package ratelimit

import "testing"

// TestSelectorMatching pins the grammar. It is net/http.ServeMux's, so there is
// nothing new to learn, with two deliberate differences noted below.
func TestSelectorMatching(t *testing.T) {
	type req struct {
		method, host, path string
		want               bool
	}
	cases := []struct {
		pattern string
		reqs    []req
	}{
		{"", []req{{"GET", "", "/", true}, {"POST", "", "/anything/at/all", true}}},
		{"/", []req{{"GET", "", "/", true}, {"POST", "", "/a/b", true}}},
		{"/api/", []req{
			{"GET", "", "/api/", true},
			{"GET", "", "/api/v1/x", true},
			// Deliberately unlike ServeMux, which would redirect /api to /api/
			// rather than match it. A control that lets a request through
			// because a trailing slash is missing is a hole.
			{"GET", "", "/api", true},
			{"GET", "", "/apiary", false},
			{"GET", "", "/other", false},
		}},
		{"/api/v1/search", []req{
			{"GET", "", "/api/v1/search", true},
			{"POST", "", "/api/v1/search", true},
			{"GET", "", "/api/v1/search/", false},
			{"GET", "", "/api/v1/searching", false},
			{"GET", "", "/api/v1", false},
		}},
		{"GET /api/things", []req{
			{"GET", "", "/api/things", true},
			{"HEAD", "", "/api/things", true}, // GET serves HEAD, as in ServeMux
			{"POST", "", "/api/things", false},
			{"DELETE", "", "/api/things", false},
		}},
		{"POST /api/things", []req{
			{"POST", "", "/api/things", true},
			{"GET", "", "/api/things", false},
			{"HEAD", "", "/api/things", false},
		}},
		{"/users/{id}", []req{
			{"GET", "", "/users/7", true},
			{"GET", "", "/users/abc", true},
			{"GET", "", "/users/", false}, // a wildcard needs a non-empty segment
			{"GET", "", "/users", false},
			{"GET", "", "/users/7/posts", false},
		}},
		{"/files/{rest...}", []req{
			{"GET", "", "/files/", true},
			{"GET", "", "/files/a", true},
			{"GET", "", "/files/a/b/c", true},
			{"GET", "", "/files", true},
			{"GET", "", "/other", false},
		}},
		{"/api/{$}", []req{
			{"GET", "", "/api/", true},
			{"GET", "", "/api", false},
			{"GET", "", "/api/x", false},
		}},
		{"example.com/api/", []req{
			{"GET", "example.com", "/api/x", true},
			{"GET", "example.com:8443", "/api/x", true}, // the port is not part of the host
			{"GET", "other.com", "/api/x", false},
			{"GET", "", "/api/x", false},
		}},
		{"GET /a/{x}/b/{y...}", []req{
			{"GET", "", "/a/1/b/2/3", true},
			{"GET", "", "/a/1/b/", true},
			{"GET", "", "/a/1/b", true},
			{"GET", "", "/a/1/c/2", false},
			{"POST", "", "/a/1/b/2", false},
		}},
	}

	for _, tc := range cases {
		sel, err := compileSelector(tc.pattern)
		if err != nil {
			t.Errorf("compile %q: %v", tc.pattern, err)
			continue
		}
		for _, r := range tc.reqs {
			p, endsSlash := normalisePath(r.path)
			got := sel.matches(r.method, r.host, p, endsSlash)
			if got != r.want {
				t.Errorf("selector %q vs %s %s%s: got %v, want %v", tc.pattern, r.method, r.host, r.path, got, r.want)
			}
		}
	}
}

// TestSelectorCannotBeEvadedByPathTricks: a selector is matched against the
// cleaned path, because the router will resolve to that path and a rule written
// for /admin has to apply to whatever reaches /admin.
func TestSelectorCannotBeEvadedByPathTricks(t *testing.T) {
	sel, err := compileSelector("/admin/")
	if err != nil {
		t.Fatal(err)
	}
	evasions := []string{
		"/admin/",
		"/admin/x",
		"//admin/x",
		"/./admin/x",
		"/api/../admin/x",
		"/admin/./x",
		"/a/b/../../admin/x",
		"///admin//x",
	}
	for _, p := range evasions {
		clean, endsSlash := normalisePath(p)
		if !sel.matches("GET", "", clean, endsSlash) {
			t.Errorf("%q normalised to %q escaped the /admin/ selector", p, clean)
		}
	}
	// And a path that genuinely is not under /admin/ must not match.
	for _, p := range []string{"/admin", "/administrator/x", "/x/admin/y"} {
		clean, endsSlash := normalisePath(p)
		if p != "/admin" && sel.matches("GET", "", clean, endsSlash) {
			t.Errorf("%q wrongly matched /admin/", p)
		}
	}
}

// TestNormalisePathDoesNotAllocateWhenClean because the common path is clean and
// this runs per request.
func TestNormalisePathDoesNotAllocateWhenClean(t *testing.T) {
	for _, p := range []string{"/", "/api/v1/things", "/api/v1/things/"} {
		n := testing.AllocsPerRun(500, func() { normalisePath(p) })
		if n != 0 {
			t.Errorf("normalisePath(%q) allocated %.2f times", p, n)
		}
	}
}

// TestOverlappingSelectorsAreAccepted is the reason this package does not use
// ServeMux for matching.
//
// A ServeMux panics when two registered patterns overlap without one being more
// specific, and a global rule alongside a narrower one is exactly that shape.
// Rules are additive: both apply.
func TestOverlappingSelectorsAreAccepted(t *testing.T) {
	lim, err := New(
		Rule{Name: "a", Selector: "/{tenant}/b", Quota: PerMinute(10)},
		Rule{Name: "b", Selector: "/a/{thing}", Quota: PerMinute(10)},
		Rule{Name: "c", Selector: "/", Quota: PerMinute(100)},
	)
	if err != nil {
		t.Fatalf("a rule table that a ServeMux would reject was refused: %v", err)
	}
	defer lim.Close()

	// And a path both narrow rules match is evaluated by both.
	matched := 0
	for _, in := range lim.Inspect(Subject{Path: "/a/b", Method: "GET"}) {
		if in.Matches {
			matched++
		}
	}
	if matched != 3 {
		t.Errorf("%d rules matched /a/b, want all 3; precedence is additive, not substitutive", matched)
	}
}

func TestStripPort(t *testing.T) {
	cases := map[string]string{
		"example.com":      "example.com",
		"example.com:8080": "example.com",
		"[::1]:8080":       "::1",
		"[::1]":            "::1",
		"::1":              "::1",
		"2001:db8::1":      "2001:db8::1",
		"1.2.3.4:80":       "1.2.3.4",
		"":                 "",
	}
	for in, want := range cases {
		if got := stripPort(in); got != want {
			t.Errorf("stripPort(%q) = %q, want %q", in, got, want)
		}
	}
}
