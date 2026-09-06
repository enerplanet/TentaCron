package e2e

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The Browser clients page quotes the example client's fetch code, so a
// reader of the guide reads what runs. Every JavaScript block of the page
// must appear in the example verbatim.
func TestBrowserGuideQuotesTheExamplePage(t *testing.T) {
	guide, err := os.ReadFile("../../docs/browser-clients.md")
	if err != nil {
		t.Fatal(err)
	}
	page, err := os.ReadFile("../../examples/browser/index.html")
	if err != nil {
		t.Fatal(err)
	}
	blocks := regexp.MustCompile("(?s)```js\n(.*?)```").FindAllStringSubmatch(string(guide), -1)
	if len(blocks) < 5 {
		t.Fatalf("the guide quotes %d JavaScript blocks; the five functions of the example page are expected", len(blocks))
	}
	for _, b := range blocks {
		snippet := strings.TrimSpace(b[1])
		if !strings.Contains(string(page), snippet) {
			first, _, _ := strings.Cut(snippet, "\n")
			t.Errorf("the guide's snippet starting %q is not in examples/browser/index.html verbatim", first)
		}
	}
}
