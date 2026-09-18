package media

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestSpoolCrashRecoveryPreservesLiveOwners(t *testing.T) {
	const childEnv = "OLP_SPOOL_CRASH_TEST_DIR"
	if base := os.Getenv(childEnv); base != "" {
		spool, err := NewSpool(base, MinCapacityBytes, nil)
		if err != nil {
			t.Fatal(err)
		}
		artifact, err := os.Create(filepath.Join(spool.root, string(NewHandle())))
		if err != nil {
			t.Fatal(err)
		}
		if err := artifact.Truncate(MinCapacityBytes); err != nil {
			t.Fatal(err)
		}
		artifact.Close()
		fmt.Println(spool.root)
		os.Exit(0) // Simulate an exit without Spool.Close; the OS releases the lock.
	}
	base := t.TempDir()
	live, err := NewSpool(base, MinCapacityBytes, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	artifact, err := live.PutBytes(t.Context(), "live.wav", "audio/wav", []byte("live"), 4)
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(os.Args[0], "-test.run=^TestSpoolCrashRecoveryPreservesLiveOwners$")
	child.Env = append(os.Environ(), childEnv+"="+base)
	output, err := child.CombinedOutput()
	if err != nil {
		t.Fatalf("crash helper: %v: %s", err, output)
	}
	abandoned := strings.TrimSpace(string(output))
	recovered, err := NewSpool(base, MinCapacityBytes, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	if _, err := os.Stat(abandoned); !os.IsNotExist(err) {
		t.Fatalf("abandoned payload remains: %v", err)
	}
	opened, err := live.Open(artifact.Handle)
	if err != nil {
		t.Fatalf("live owner's artifact removed: %v", err)
	}
	opened.File.Close()
	if recovered.UsedBytes() != 0 {
		t.Fatal("other processes consume this spool's capacity")
	}
	if _, err := recovered.PutBytes(t.Context(), "new.wav", "audio/wav", []byte("new"), 3); err != nil {
		t.Fatalf("restart cannot accept uploads: %v", err)
	}
}

func TestSpoolRecoversLegacyDeadProcess(t *testing.T) {
	base := t.TempDir()
	abandoned := filepath.Join(base, "olp-media-2147483647-dead")
	if err := os.Mkdir(abandoned, 0700); err != nil {
		t.Fatal(err)
	}
	artifact, err := os.Create(filepath.Join(abandoned, string(NewHandle())))
	if err != nil {
		t.Fatal(err)
	}
	if err := artifact.Truncate(MinCapacityBytes); err != nil {
		t.Fatal(err)
	}
	artifact.Close()
	spool, err := NewSpool(base, MinCapacityBytes, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer spool.Close()
	if _, err := os.Stat(abandoned); !os.IsNotExist(err) {
		t.Fatal("legacy payload was not reclaimed")
	}
	if _, err := spool.PutBytes(t.Context(), "new.wav", "audio/wav", []byte("new"), 3); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentSpoolRegistrationPreservesLiveDirectories(t *testing.T) {
	base := t.TempDir()
	spools := make(chan *Spool, 8)
	var wg sync.WaitGroup
	for range cap(spools) {
		wg.Go(func() {
			spool, err := NewSpool(base, MinCapacityBytes, nil)
			if err != nil {
				t.Error(err)
				return
			}
			spools <- spool
		})
	}
	wg.Wait()
	close(spools)
	for spool := range spools {
		if _, err := spool.PutBytes(t.Context(), "live", "application/octet-stream", []byte("live"), 4); err != nil {
			t.Errorf("concurrent recovery removed live spool: %v", err)
		}
		spool.Close()
	}
}
