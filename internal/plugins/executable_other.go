//go:build !linux

package plugins

import (
	"errors"
	"os"
	"os/exec"
)

// Unconfined plugins run only on Linux, OLP's release platform: OLP runs a
// sealed memory copy of the executable it hashed.

var errUnsupported = errors.New("unconfined plugins run only on Linux")

func sealed(string) (*os.File, string, error) { return nil, "", errUnsupported }

func command(*os.File, string) *exec.Cmd { return nil }

func exited(int) error { return errUnsupported }

func kill(int) {}
