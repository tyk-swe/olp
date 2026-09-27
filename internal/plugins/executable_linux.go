package plugins

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

// sealed copies the executable at path into a sealed memory file and returns
// the copy with the digest of what it copied. OLP runs the copy, so what runs
// is what OLP hashed, however the file in the directory changes meanwhile.
func sealed(path string) (*os.File, string, error) {
	source, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	defer source.Close()
	flags := unix.MFD_CLOEXEC | unix.MFD_ALLOW_SEALING
	fd, err := unix.MemfdCreate("olp-plugin", flags|unix.MFD_EXEC)
	if errors.Is(err, unix.EINVAL) {
		// Kernels before 6.3 know no MFD_EXEC, and their memory files are
		// executable.
		fd, err = unix.MemfdCreate("olp-plugin", flags)
	}
	if err != nil {
		return nil, "", err
	}
	copied := os.NewFile(uintptr(fd), "olp-plugin")
	sum := sha256.New()
	if _, err = io.Copy(io.MultiWriter(copied, sum), source); err == nil {
		_, err = unix.FcntlInt(copied.Fd(), unix.F_ADD_SEALS, unix.F_SEAL_SEAL|unix.F_SEAL_SHRINK|unix.F_SEAL_GROW|unix.F_SEAL_WRITE)
	}
	if err != nil {
		copied.Close()
		return nil, "", err
	}
	return copied, hex.EncodeToString(sum.Sum(nil)), nil
}

// command runs the sealed copy of an executable, which the process receives
// as its descriptor 3, in a process group of its own, so stopping the plugin
// stops whatever it started.
func command(copied *os.File, name string) *exec.Cmd {
	return &exec.Cmd{
		Path:        "/proc/self/fd/3",
		Args:        []string{name},
		ExtraFiles:  []*os.File{copied},
		SysProcAttr: &syscall.SysProcAttr{Setpgid: true},
	}
}

// exited waits for the process to exit, and leaves it unreaped: until it is
// reaped, its ID still names its process group.
func exited(pid int) error {
	for {
		var info unix.Siginfo
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}

// kill kills the process group whose leader has pid.
func kill(pid int) { _ = unix.Kill(-pid, unix.SIGKILL) }
