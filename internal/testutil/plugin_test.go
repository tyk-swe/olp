package testutil

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestPluginArtifactsIsolateConcurrentCallersAndBuildVariants(t *testing.T) {
	const pkg = "./internal/plugins/testdata/fixture"
	modules := make([][]byte, 8)
	var callers sync.WaitGroup
	for i := range modules {
		callers.Go(func() { modules[i] = BuildPlugin(t, pkg) })
	}
	callers.Wait()
	for _, module := range modules {
		if !bytes.Equal(module, modules[0]) || !bytes.HasPrefix(module, []byte("\x00asm")) {
			t.Fatal("concurrent callers received different or non-Wasm artifacts")
		}
	}
	modules[0][0] = 0xff
	if !bytes.Equal(BuildPlugin(t, pkg), modules[1]) {
		t.Fatal("mutating a returned module changed the cache or another caller")
	}
	if bytes.Equal(BuildPlugin(t, pkg, "-X=main.behaviour=variant"), modules[1]) {
		t.Fatal("linker variants shared an artifact")
	}
	if bytes.Equal(build(t, pkg, "wasip1", "wasm", []string{"-buildmode=c-shared", "-gcflags=-l"}, nil), modules[1]) {
		t.Fatal("build flag variants shared an artifact")
	}
	if bytes.Equal(BuildPlugin(t, "./internal/plugins/testdata/otherabi"), modules[1]) {
		t.Fatal("different packages shared an artifact")
	}
	first, second := filepath.Join(t.TempDir(), "first"), filepath.Join(t.TempDir(), "second")
	BuildExecutablePlugin(t, first, pkg)
	BuildExecutablePlugin(t, second, pkg)
	executable, err := os.ReadFile(second)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(executable, modules[1]) || bytes.HasPrefix(executable, []byte("\x00asm")) {
		t.Fatal("native and Wasm targets shared an artifact")
	}
	if err := os.WriteFile(first, []byte("corrupt"), 0700); err != nil {
		t.Fatal(err)
	}
	unchanged, err := os.ReadFile(second)
	if err != nil || !bytes.Equal(unchanged, executable) {
		t.Fatal("executables did not receive independent copies")
	}
	BuildExecutablePlugin(t, first, pkg)
	rebuilt, err := os.ReadFile(first)
	if err != nil || !bytes.Equal(rebuilt, executable) {
		t.Fatal("mutating an executable changed the cached artifact")
	}
}
