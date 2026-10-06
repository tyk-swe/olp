// Command pluginindex builds the plugins the reviewed plugin index lists from
// this repository, reproducibly, and compares them with the index.
//
//	pluginindex entry DIR...    prints the index release of each plugin source directory
//	pluginindex check OUT_DIR   builds every indexed plugin into OUT_DIR and fails
//	                            unless each newest release matches its build
//
// Builds are reproducible: the same source and Go toolchain give the same
// module digest. A release runs check and publishes the modules it built, so
// the digests an owner installs are the digests the index lists.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/pluginindex"
	"github.com/tyk-swe/olp/internal/plugins"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "pluginindex:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	if len(args) < 2 || args[0] != "entry" && args[0] != "check" {
		return errors.New("usage: pluginindex entry DIR... | check OUT_DIR")
	}
	ctx := context.Background()
	runtime, err := plugins.NewRuntime(ctx, plugins.Compiled, plugins.DefaultLimits, slog.New(slog.DiscardHandler))
	if err != nil {
		return err
	}
	defer runtime.Close(ctx)
	if args[0] == "entry" {
		return entries(ctx, runtime, args[1:], stdout)
	}
	return check(ctx, runtime, args[1], stdout)
}

// built is a plugin module built from source and the manifest it declares.
type built struct {
	module   []byte
	manifest abi.Manifest
}

// build compiles a plugin source directory as a WASI reactor, reproducibly,
// and inspects it as an install does.
func build(ctx context.Context, runtime *plugins.Runtime, dir string) (built, error) {
	output, err := os.CreateTemp("", "olp-plugin-*.wasm")
	if err != nil {
		return built{}, err
	}
	output.Close()
	defer os.Remove(output.Name())
	command := exec.CommandContext(ctx, "go", "build", "-mod=readonly", "-trimpath", "-buildvcs=false", "-buildmode=c-shared", "-o", output.Name(), "./"+filepath.ToSlash(filepath.Clean(dir)))
	command.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm", "CGO_ENABLED=0", "GOFLAGS=")
	if out, err := command.CombinedOutput(); err != nil {
		return built{}, fmt.Errorf("building %s: %v\n%s", dir, err, out)
	}
	module, err := os.ReadFile(output.Name())
	if err != nil {
		return built{}, err
	}
	manifest, err := runtime.Inspect(ctx, module)
	if err != nil {
		return built{}, fmt.Errorf("%s: %w", dir, err)
	}
	return built{module: module, manifest: manifest}, nil
}

func (b built) release(commit string, reviewed time.Time) pluginindex.Release {
	digest := sha256.Sum256(b.module)
	var profiles []string
	for _, profile := range b.manifest.Profiles {
		profiles = append(profiles, profile.ID)
	}
	return pluginindex.Release{Version: b.manifest.Version, Digest: hex.EncodeToString(digest[:]), ABIVersion: abi.Version, SizeBytes: int64(len(b.module)),
		Origins: b.manifest.Origins, Profiles: profiles, Commit: commit, ReviewedAt: reviewed}
}

// headCommit is the revision a release builds from.
func headCommit(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "rev-parse", "HEAD").Output()
	return strings.TrimSpace(string(out)), err
}

func entries(ctx context.Context, runtime *plugins.Runtime, dirs []string, stdout io.Writer) error {
	commit, err := headCommit(ctx)
	if err != nil {
		return err
	}
	for _, dir := range dirs {
		b, err := build(ctx, runtime, dir)
		if err != nil {
			return err
		}
		entry := struct {
			Name        string              `json:"name"`
			Description string              `json:"description"`
			Path        string              `json:"path"`
			Release     pluginindex.Release `json:"release"`
		}{b.manifest.Name, b.manifest.Description, filepath.ToSlash(filepath.Clean(dir)), b.release(commit, time.Now().UTC().Truncate(time.Second))}
		encoded, _ := json.MarshalIndent(entry, "", "  ")
		fmt.Fprintln(stdout, string(encoded))
	}
	return nil
}

func check(ctx context.Context, runtime *plugins.Runtime, out string, stdout io.Writer) error {
	signed, err := pluginindex.Embedded()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	for _, listed := range signed.Index.Plugins {
		newest := listed.Releases[0]
		b, err := build(ctx, runtime, listed.Path)
		if err != nil {
			return err
		}
		got := b.release(newest.Commit, newest.ReviewedAt)
		got.Origins, got.Profiles = slices.Sorted(slices.Values(got.Origins)), slices.Sorted(slices.Values(got.Profiles))
		if b.manifest.Name != listed.Name || got.Version != newest.Version || got.Digest != newest.Digest || got.SizeBytes != newest.SizeBytes ||
			!slices.Equal(got.Origins, newest.Origins) || !slices.Equal(got.Profiles, newest.Profiles) {
			return fmt.Errorf("%s builds as %s %s (%s); the index lists %s (%s). Review the build and add it as a release", listed.Path, b.manifest.Name, got.Version, got.Digest, newest.Version, newest.Digest)
		}
		name := filepath.Join(out, listed.Name+"-"+got.Version+".wasm")
		if err := os.WriteFile(name, b.module, 0o644); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "%s %s matches the index: %s\n", listed.Name, got.Version, got.Digest)
	}
	return nil
}
