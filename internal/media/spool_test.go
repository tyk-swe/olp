package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func testSpool(t *testing.T, capacity int64) *Spool {
	t.Helper()
	spool, err := NewSpool(t.TempDir(), capacity, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { spool.Close() })
	return spool
}

func TestSpoolPutOpenRemoveLifecycle(t *testing.T) {
	spool := testSpool(t, MinCapacityBytes)
	payload := strings.Repeat("media-bytes", 4096)
	artifact, err := spool.Put(context.Background(), Upload{
		Filename:      "upload.bin",
		ContentType:   "application/octet-stream",
		MaximumLength: int64(len(payload)),
		Body:          io.NopCloser(strings.NewReader(payload)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Handle == "" || artifact.ContentLength != int64(len(payload)) {
		t.Fatalf("artifact: %+v", artifact)
	}
	expected := sha256.Sum256([]byte(payload))
	if artifact.Digest != hex.EncodeToString(expected[:]) {
		t.Fatal("spool did not retain the original byte digest")
	}
	ref, err := artifact.BlobReference()
	if err != nil || ref.ID() != string(artifact.Handle) || ref.Digest() != artifact.Digest || ref.Size() != artifact.ContentLength || ref.MediaType() != "application/octet-stream" {
		t.Fatal("staged artifact has no complete OIF byte identity", err)
	}
	if err := ValidateHandle(string(artifact.Handle)); err != nil {
		t.Fatal(err)
	}
	opened, err := spool.Open(artifact.Handle)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(opened.File)
	opened.File.Close()
	if err != nil || string(data) != payload {
		t.Fatalf("readback mismatch: %v", err)
	}
	if opened.Artifact.Digest != artifact.Digest {
		t.Fatal("spool reopened with a different byte identity")
	}
	if err := spool.Remove(artifact.Handle); err != nil {
		t.Fatal(err)
	}
	if _, err := spool.Open(artifact.Handle); err == nil {
		t.Fatal("removed handle still opens")
	}
}

func TestSpoolDigestCoversEveryChunkAndPreservesMissingContentType(t *testing.T) {
	spool := testSpool(t, MinCapacityBytes)
	payload := strings.Repeat("three-byte-chunks", 16385)
	artifact, err := spool.Put(t.Context(), Upload{
		Filename: "unknown.bin", MaximumLength: int64(len(payload)),
		Body: strings.NewReader(payload),
	})
	if err != nil {
		t.Fatal(err)
	}
	expected := sha256.Sum256([]byte(payload))
	if artifact.Digest != hex.EncodeToString(expected[:]) || artifact.ContentType != "" {
		t.Fatal("multi-chunk bytes or caller MIME presence changed")
	}
	ref, err := artifact.BlobReference()
	if err != nil || ref.MediaType() != "application/octet-stream" || ref.Digest() != artifact.Digest {
		t.Fatal("missing MIME could not use a neutral storage byte type", err)
	}
	if err := spool.Remove(artifact.Handle); err != nil {
		t.Fatal(err)
	}
}

func TestSpoolRejectsOversizedAndOverCapacityUploads(t *testing.T) {
	spool := testSpool(t, MinCapacityBytes)
	if _, err := spool.Put(context.Background(), Upload{
		Filename:      "big.bin",
		MaximumLength: 64,
		Body:          io.NopCloser(strings.NewReader(strings.Repeat("x", 65))),
	}); !TooLarge(err) {
		t.Fatalf("expected TooLarge, got %v", err)
	}
	if spool.tryReserve(spool.CapacityBytes() + 1) {
		t.Fatal("capacity overflow reserved")
	}
	if !spool.tryReserve(1 << 20) {
		t.Fatal("in-capacity reservation refused")
	}
	spool.release(1 << 20)
}

func TestSpoolRejectsInvalidHandles(t *testing.T) {
	spool := testSpool(t, MinCapacityBytes)
	for _, handle := range []string{"", "../escape", "not-a-handle", strings.Repeat("f", 200)} {
		if _, err := spool.Open(Handle(handle)); err == nil {
			t.Fatalf("invalid handle %q opened", handle)
		}
	}
}

func TestSpoolSanitizesFilenames(t *testing.T) {
	if safe, err := SafeFilename("../../etc/passwd"); err != nil || safe != "passwd" {
		t.Fatalf("traversal should reduce to basename, got %q %v", safe, err)
	}
	if _, err := SafeFilename(""); err == nil {
		t.Fatal("empty filename accepted")
	}
	if _, err := SafeFilename(".."); err == nil {
		t.Fatal("dot-dot filename accepted")
	}
	if safe, err := SafeFilename("nested/dir/upload.png"); err != nil || safe != "upload.png" {
		t.Fatalf("nested filename: %q %v", safe, err)
	}
	if _, err := SafeFilename("bad\x00name"); err == nil {
		t.Fatal("NUL filename accepted")
	}
}

func TestSpoolAbortedWriteReleasesCapacity(t *testing.T) {
	spool := testSpool(t, MinCapacityBytes)
	before := spool.UsedBytes()
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	go func() {
		_, err := spool.Put(context.Background(), Upload{
			Filename:      "stream.bin",
			MaximumLength: 1 << 20,
			Body:          reader,
		})
		done <- err
	}()
	writer.Write(bytes.Repeat([]byte("chunk"), 1000))
	writer.CloseWithError(errors.New("client aborted"))
	if err := <-done; err == nil {
		t.Fatal("aborted upload staged")
	}
	deadline := time.Now().Add(2 * time.Second)
	for spool.UsedBytes() != before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if spool.UsedBytes() != before {
		t.Fatalf("capacity leaked: %d != %d", spool.UsedBytes(), before)
	}
}
