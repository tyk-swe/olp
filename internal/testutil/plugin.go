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
	out := filepath.Join(t.TempDir(), "plugin.wasm")
	build(t, out, pkg, []string{"-buildmode=c-shared"}, []string{"GOOS=wasip1", "GOARCH=wasm"}, ldflags)
	module, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return module
}

// BuildExecutablePlugin builds a provider plugin package of this module
// natively, as an unconfined plugin's executable at path, passing ldflags, if
// any, to the linker.
func BuildExecutablePlugin(t testing.TB, path, pkg string, ldflags ...string) {
	t.Helper()
	build(t, path, pkg, nil, nil, ldflags)
}

func build(t testing.TB, out, pkg string, flags, env, ldflags []string) {
	t.Helper()
	root, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}").Output()
	if err != nil {
		t.Fatal(err)
	}
	args := append([]string{"build"}, flags...)
	cmd := exec.Command("go", append(args, "-ldflags", strings.Join(ldflags, " "), "-o", out, pkg)...)
	cmd.Dir = strings.TrimSpace(string(root))
	cmd.Env = append(append(os.Environ(), env...), "CGO_ENABLED=0")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build plugin %s: %v\n%s", pkg, err, output)
	}
}
