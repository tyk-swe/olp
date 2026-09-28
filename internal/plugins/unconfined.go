package plugins

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"

	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// Unconfined is the experimental unconfined tier of a deployment that enables
// it (ADR 0007): the directory of its image that holds the executables of
// unconfined plugins. OLP runs an unconfined plugin as a subprocess with the
// operating system's privileges, speaking the plugin ABI over standard input
// and output. Only a deployment setting names the directory, so nothing the
// management API does enables the tier; an owner then permits each plugin.
type Unconfined struct {
	dir    string
	limits Limits
	log    *slog.Logger
}

// NewUnconfined returns the unconfined tier whose executables live in dir,
// whose calls stay within limits' time and log to log.
func NewUnconfined(dir string, limits Limits, log *slog.Logger) *Unconfined {
	return &Unconfined{dir: dir, limits: limits, log: log}
}

// executableName is the form of the executables' file names OLP lists.
var executableName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// An ExecutableFile is an executable in the unconfined plugin directory.
type ExecutableFile struct {
	// Name is the executable's file name in the directory.
	Name string
	// Digest is the lowercase hexadecimal SHA-256 digest of the executable,
	// which a permitted plugin is identified by and revisions pin.
	Digest string
	Size   int64
}

// Executables lists the executables in the unconfined plugin directory, by
// name: regular files with an execute permission, whose names are 1–128
// letters, digits, dots, underscores and hyphens, not starting with a dot,
// underscore or hyphen. Listing runs none of them.
func (u *Unconfined) Executables() ([]ExecutableFile, error) {
	entries, err := os.ReadDir(u.dir)
	if err != nil {
		return nil, err
	}
	files := []ExecutableFile{}
	for _, entry := range entries {
		file, err := u.executable(entry.Name())
		if refusal, ok := errors.AsType[*Error](err); ok && refusal.Code == CodeExecutableUnknown {
			continue
		}
		if err != nil {
			return nil, err
		}
		files = append(files, file)
	}
	return files, nil
}

// executable describes the executable with name, which it refuses with
// CodeExecutableUnknown unless it is one Executables lists.
func (u *Unconfined) executable(name string) (ExecutableFile, error) {
	path, info, err := u.lookup(name)
	if err != nil {
		return ExecutableFile{}, err
	}
	digest, err := fileDigest(path)
	return ExecutableFile{Name: name, Digest: digest, Size: info.Size()}, err
}

// lookup returns the path and file information of the executable with name,
// which it refuses with CodeExecutableUnknown unless it is one Executables
// lists.
func (u *Unconfined) lookup(name string) (string, fs.FileInfo, error) {
	unknown := refuse(CodeExecutableUnknown, "The unconfined plugin directory holds no executable with this name.")
	if !executableName.MatchString(name) {
		return "", nil, unknown
	}
	path := filepath.Join(u.dir, name)
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) || err == nil && (!info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0) {
		return "", nil, unknown
	}
	return path, info, err
}

func fileDigest(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	sum := sha256.New()
	if _, err = io.Copy(sum, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// Inspect runs the executable with name and returns it with the manifest it
// declares. It refuses an executable that doesn't speak the plugin ABI over
// standard input and output, was built for another ABI version, declares an
// invalid manifest or names a dialect OLP doesn't have.
func (u *Unconfined) Inspect(ctx context.Context, name string) (ExecutableFile, abi.Manifest, error) {
	file, err := u.executable(name)
	if err != nil {
		return file, abi.Manifest{}, err
	}
	executable := u.Load(file.Digest, name)
	defer executable.Close(context.WithoutCancel(ctx))
	manifest, err := inspect(ctx, executable, true)
	return file, manifest, err
}

// Load returns the unconfined plugin whose executable has name and digest.
// It starts nothing until the first call.
func (u *Unconfined) Load(digest, name string) *Executable {
	return &Executable{Digest: digest, name: name, tier: u}
}
