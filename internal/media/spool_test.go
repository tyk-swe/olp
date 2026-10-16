package media

import (
	"testing"
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
