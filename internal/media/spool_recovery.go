package media

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

const spoolOwnerFile = ".owner.lock"

// Serialize recovery with directory registration: another process must never
// mistake a newly created directory for an abandoned, unlocked spool.
func openSpoolDirectory(base string) (string, *os.File, error) {
	recovery, err := os.OpenFile(filepath.Join(base, ".recovery.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return "", nil, err
	}
	defer recovery.Close()
	if err := unix.Flock(int(recovery.Fd()), unix.LOCK_EX); err != nil {
		return "", nil, err
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		return "", nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "olp-media-") {
			continue
		}
		if err := reclaimSpool(filepath.Join(base, entry.Name()), entry.Name()); err != nil {
			return "", nil, err
		}
	}
	root := filepath.Join(base, fmt.Sprintf("olp-media-%d-%s", os.Getpid(), uuid.Must(uuid.NewV7())))
	if err := os.Mkdir(root, 0700); err != nil {
		return "", nil, err
	}
	owner, err := os.OpenFile(filepath.Join(root, spoolOwnerFile), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		os.RemoveAll(root)
		return "", nil, err
	}
	if err := unix.Flock(int(owner.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		owner.Close()
		os.RemoveAll(root)
		return "", nil, err
	}
	return root, owner, nil
}

func reclaimSpool(root, name string) error {
	owner, err := os.OpenFile(filepath.Join(root, spoolOwnerFile), os.O_RDWR, 0)
	if errors.Is(err, os.ErrNotExist) {
		// Older releases have no ownership lock. Preserve a live or unidentifiable
		// process, and reclaim only directories whose recorded PID no longer exists.
		pidText, _, ok := strings.Cut(strings.TrimPrefix(name, "olp-media-"), "-")
		pid, parseErr := strconv.Atoi(pidText)
		if !ok || parseErr != nil || pid <= 0 {
			return nil
		}
		if !errors.Is(unix.Kill(pid, 0), unix.ESRCH) {
			return nil
		}
		return os.RemoveAll(root)
	}
	if err != nil {
		return err
	}
	defer owner.Close()
	if err := unix.Flock(int(owner.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil
		}
		return err
	}
	// The kernel releases the ownership lock on exit, including SIGKILL. Keep
	// it until removal finishes so concurrent cleanup cannot claim these files.
	return os.RemoveAll(root)
}
