package api

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/enerplanet/tentacron/internal/cors"
)

// The Fetch specification lets a page read these response headers without
// them being exposed, and send these request headers without a preflight
// naming them.
var (
	safelistedResponseHeaders = []string{"Cache-Control", "Content-Language", "Content-Length", "Content-Type", "Expires", "Last-Modified", "Pragma"}
	safelistedRequestHeaders  = []string{"Accept", "Accept-Language", "Content-Language", "Content-Type"}
	// corsProtocolHeaders are read by the browser policy itself, never by a
	// page.
	corsProtocolHeaders = []string{"Origin", "Access-Control-Request-Method", "Access-Control-Request-Headers", "Access-Control-Request-Private-Network"}
)

var (
	setHeaderRE  = regexp.MustCompile(`Header\(\)\.(?:Set|Add)\("([A-Za-z][A-Za-z0-9-]*)"`)
	readHeaderRE = regexp.MustCompile(`\.Header\.(?:Get|Values)\("([A-Za-z][A-Za-z0-9-]*)"`)
)

// headerLiterals scans the package's non-test sources for header names.
func headerLiterals(t *testing.T, re *regexp.Regexp) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Clean(e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range re.FindAllStringSubmatch(string(src), -1) {
			name := http.CanonicalHeaderKey(m[1])
			if !slices.Contains(names, name) {
				names = append(names, name)
			}
		}
	}
	if len(names) == 0 {
		t.Fatal("no header literal found; the scan pattern is broken")
	}
	return names
}

func canonical(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = http.CanonicalHeaderKey(n)
	}
	return out
}

// Every response header a handler sets is readable from a page: either
// safelisted by the Fetch specification or in the exposed set. The next
// header added to a handler fails here instead of silently being
// unreadable from a browser.
func TestEveryResponseHeaderIsReadableFromABrowser(t *testing.T) {
	readable := append(canonical(cors.DefaultExposeHeaders), safelistedResponseHeaders...)
	for _, name := range headerLiterals(t, setHeaderRE) {
		if !slices.Contains(readable, name) {
			t.Errorf("handlers set %s, which a page cannot read: add it to cors.DefaultExposeHeaders", name)
		}
	}
}

// Every request header a handler reads is allowed on a preflight, so a
// page can send it.
func TestEveryRequestHeaderIsAllowedOnPreflights(t *testing.T) {
	allowed := append(canonical(cors.DefaultAllowedHeaders), safelistedRequestHeaders...)
	allowed = append(allowed, corsProtocolHeaders...)
	for _, name := range headerLiterals(t, readHeaderRE) {
		if !slices.Contains(allowed, name) {
			t.Errorf("handlers read %s, which a preflight does not allow: add it to cors.DefaultAllowedHeaders", name)
		}
	}
}
