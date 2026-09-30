package testutil

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

type pluginBuildKey struct {
	pkg, goos, goarch, flags, ldflags, goflags string
}

var pluginArtifacts sync.Map

var moduleRoot = sync.OnceValues(func() (string, error) {
	root, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}").Output()
	return strings.TrimSpace(string(root)), err
})

// BuildPlugin builds a provider plugin package of this module, such as
// ./sdk/plugin/reference, for wasip1 and returns the module. ldflags, if any,
// are passed to the linker, so one source package can build several fixtures.
func BuildPlugin(t testing.TB, pkg string, ldflags ...string) []byte {
	t.Helper()
	return build(t, pkg, "wasip1", "wasm", []string{"-buildmode=c-shared"}, ldflags)
}

// BuildExecutablePlugin builds a provider plugin package of this module
// natively, as an unconfined plugin's executable at path, passing ldflags, if
// any, to the linker.
func BuildExecutablePlugin(t testing.TB, path, pkg string, ldflags ...string) {
	t.Helper()
	goos, goarch := os.Getenv("GOOS"), os.Getenv("GOARCH")
	if goos == "" {
		goos = runtime.GOOS
	}
	if goarch == "" {
		goarch = runtime.GOARCH
	}
	if err := os.WriteFile(path, build(t, pkg, goos, goarch, nil, ldflags), 0700); err != nil {
		t.Fatal(err)
	}
}

func build(t testing.TB, pkg, goos, goarch string, flags, ldflags []string) []byte {
	t.Helper()
	key := pluginBuildKey{pkg, goos, goarch, strings.Join(flags, "\x00"), strings.Join(ldflags, " "), os.Getenv("GOFLAGS")}
	artifact, _ := pluginArtifacts.LoadOrStore(key, sync.OnceValues(func() ([]byte, error) {
		root, err := moduleRoot()
		if err != nil {
			return nil, err
		}
		dir, err := os.MkdirTemp("", "olp-plugin-build-")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(dir)
		out := filepath.Join(dir, "plugin")
		args := append([]string{"build", "-mod=readonly"}, flags...)
		cmd := exec.Command("go", append(args, "-ldflags", key.ldflags, "-o", out, pkg)...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED=0")
		if output, err := cmd.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("build plugin %s: %w\n%s", pkg, err, output)
		}
		return os.ReadFile(out)
	}))
	module, err := artifact.(func() ([]byte, error))()
	if err != nil {
		t.Fatal(err)
	}
	// Only immutable build results are shared. Callers may corrupt a module or
	// replace an executable without changing another test's artifact.
	return bytes.Clone(module)
}
