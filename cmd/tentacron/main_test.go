package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validConfig = `
auth:
  api_keys: [{name: frontend, key: "${TC_CLI_TEST_KEY}"}]
targets:
  demo:
    url: "https://demo.example.com/run"
  ignis:
    url: "https://ignis.example.com/calc/{code}"
    proxy: true
resolvents:
  resolvent-pv1:
    url: "https://pv.example.com/gen"
    api_key: "${TC_CLI_TEST_KEY}"
`

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func runCLI(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = run(args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestValidateAcceptsAGoodConfigAndDescribesIt(t *testing.T) {
	t.Setenv("TC_CLI_TEST_KEY", "s3cret-value")
	code, out, errOut := runCLI(t, "validate", "-config", writeConfig(t, validConfig))
	if code != 0 || errOut != "" {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	for _, want := range []string{"configuration ok", "2 target(s)", "1 resolvent type(s)",
		"demo", "POST https://demo.example.com/run", "ignis", "proxy", "resolvent-pv1", "key via header X-API-Key"} {
		if !strings.Contains(out, want) {
			t.Errorf("summary lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "s3cret-value") {
		t.Errorf("summary must never print credentials:\n%s", out)
	}
}

func TestValidateReportsEveryProblemAndExits1(t *testing.T) {
	t.Setenv("TC_CLI_TEST_KEY", "k")
	bad := strings.Replace(validConfig, `url: "https://demo.example.com/run"`, "url: \"nope\"\n    method: DELETE", 1)
	code, out, errOut := runCLI(t, "validate", "-config", writeConfig(t, bad))
	if code != 1 || out != "" {
		t.Fatalf("exit %d, stdout %q", code, out)
	}
	for _, want := range []string{"configuration is invalid", "  - targets.demo.url", "  - targets.demo.method"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr lacks %q:\n%s", want, errOut)
		}
	}
}

func TestValidateNamesUnsetEnvironmentVariables(t *testing.T) {
	_ = os.Unsetenv("TC_CLI_TEST_KEY") // the case under test is the variable being absent
	code, _, errOut := runCLI(t, "validate", "-config", writeConfig(t, validConfig))
	if code != 1 || !strings.Contains(errOut, "TC_CLI_TEST_KEY") {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
}

func TestDispatchAndUsage(t *testing.T) {
	if code, out, _ := runCLI(t, "version"); code != 0 || !strings.HasPrefix(out, "tentacron dev go") {
		t.Errorf("version: exit %d, out %q", code, out)
	}
	if code, out, _ := runCLI(t, "help"); code != 0 || !strings.Contains(out, "Usage:") {
		t.Errorf("help: exit %d, out %q", code, out)
	}
	if code, _, errOut := runCLI(t, "frobnicate"); code != 2 || !strings.Contains(errOut, `unknown command "frobnicate"`) {
		t.Errorf("unknown command: exit %d, stderr %q", code, errOut)
	}
	if code, _, errOut := runCLI(t, "validate", "-config", "x.yaml", "extra"); code != 2 || !strings.Contains(errOut, "expected 0 argument(s), got 1") {
		t.Errorf("stray argument: exit %d, stderr %q", code, errOut)
	}
	if code, out, _ := runCLI(t, "validate", "-h"); code != 0 || !strings.Contains(out, "Usage:") {
		t.Errorf("validate -h: exit %d, out %q", code, out)
	}
	if code, _, errOut := runCLI(t, "validate", "-bogus"); code != 2 || !strings.Contains(errOut, "flag provided but not defined") {
		t.Errorf("unknown flag: exit %d, stderr %q", code, errOut)
	}
}

// A leading flag still means serve, and a missing config file fails before
// anything listens.
func TestDefaultCommandIsServe(t *testing.T) {
	code, _, _ := runCLI(t, "-config", filepath.Join(t.TempDir(), "missing.yaml"))
	if code != 1 {
		t.Fatalf("serve with a missing config: exit %d, want 1", code)
	}
	if code, out, _ := runCLI(t, "serve", "-h"); code != 0 || !strings.Contains(out, "Usage:") {
		t.Errorf("serve -h: exit %d, out %q", code, out)
	}
}
