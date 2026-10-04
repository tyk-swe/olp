package estimate

import (
	"strings"
	"sync"
	"testing"
)

// TestConcurrentUseOfOneEncoding races the first load and then the shared
// state, the lookup tables and the scratch pool, from many goroutines, with
// results that must match a single-threaded run. Run it under -race.
func TestConcurrentUseOfOneEncoding(t *testing.T) {
	texts := []string{
		"hello world",
		strings.Repeat("The quick brown fox jumps over the lazy dog. ", 200),
		strings.Repeat("a", 3_000),
		strings.Repeat("\U00004eca\U00005929\U00005929\U00006c14\U00005f88\U0000597d", 400),
		strings.Repeat(" ", 5_000) + "x",
	}
	fresh := &Encoding{name: "o200k_base", ranks: o200kRanks, scan: scanO200k}
	want := make([]int, len(texts))
	for i, text := range texts {
		n, err := O200kBase.Count(text)
		if err != nil {
			t.Fatal(err)
		}
		want[i] = n
	}
	var wg sync.WaitGroup
	for g := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for round := range 20 {
				i := (g + round) % len(texts)
				if got, err := fresh.Count(texts[i]); err != nil || got != want[i] {
					t.Errorf("goroutine %d: Count(text %d) = %d, %v; want %d", g, i, got, err, want[i])
					return
				}
				m := ForModel("gpt-4o").Meter()
				m.Add(texts[i])
				m.Total()
			}
		}()
	}
	wg.Wait()
}
