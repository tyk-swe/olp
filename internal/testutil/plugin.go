package testutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// BuildPlugin builds a provider plugin package of this module, such as
// ./sdk/plugin/reference, for wasip1 and returns the module. ldflags, if any,
// are passed to the linker, so one source package can build several fixtures.
func BuildPlugin(t testing.TB, pkg string, ldflags ...string) []byte {
	t.Helper()
	root, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}").Output()
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "plugin.wasm")
	cmd := exec.Command("go", "build", "-buildmode=c-shared", "-ldflags", strings.Join(ldflags, " "), "-o", out, pkg)
	cmd.Dir = strings.TrimSpace(string(root))
	cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm", "CGO_ENABLED=0")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build plugin %s: %v\n%s", pkg, err, output)
	}
	module, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return module
}
