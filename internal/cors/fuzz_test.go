package cors

import (
	"strings"
	"testing"
)

// referenceAllowed is the slow, obvious reading of the rules the fuzz
// target checks the fast matcher against.
func referenceAllowed(entries []string, origin string) bool {
	o := strings.ToLower(origin)
	for _, e := range entries {
		e = strings.ToLower(e)
		switch {
		case e == "*":
			return true
		case strings.Contains(e, "://*."):
			scheme, host, _ := strings.Cut(e, "://*.")
			prefix, suffix := scheme+"://", "."+host
			if strings.HasPrefix(o, prefix) && strings.HasSuffix(o, suffix) && len(o) > len(prefix)+len(suffix) {
				return true
			}
		case e == o:
			return true
		}
	}
	return false
}

// The Origin header is untrusted input: the matcher must never panic and
// never allow what the reference reading of the rules refuses.
func FuzzOriginAllowed(f *testing.F) {
	entries := []string{"https://app.example.org", "https://*.preview.example.org", "null", "http://localhost:5173"}
	for _, seed := range []string{"https://app.example.org", "https://x.preview.example.org", "https://preview.example.org", "null", "", "*", "https://evil-example.org", "HTTPS://APP.EXAMPLE.ORG", "https://.preview.example.org", strings.Repeat("a", 5000)} {
		f.Add(seed)
	}
	p := newPolicy(Config{AllowedOrigins: entries})
	f.Fuzz(func(t *testing.T, origin string) {
		if got, want := p.originAllowed(origin), referenceAllowed(entries, origin); got != want {
			t.Fatalf("origin %q: allowed = %v, reference says %v", origin, got, want)
		}
	})
}

// ValidateOrigin must not panic on any input.
func FuzzValidateOrigin(f *testing.F) {
	for _, seed := range []string{"*", "null", "https://*.example.org", "https://x", "://", "https://", "*.", "https://*.", "a://*"} {
		f.Add(seed)
	}
	f.Fuzz(func(_ *testing.T, origin string) {
		_ = ValidateOrigin(origin)
		_ = newPolicy(Config{AllowedOrigins: []string{origin}}).originAllowed(origin)
	})
}
