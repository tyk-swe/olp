package openaistate

import (
	"strconv"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/vendors"
)

func TestBatchCompletionWindowAdmitsExactlyTheVendorRange(t *testing.T) {
	t.Parallel()
	for _, contract := range vendors.All() {
		served, _ := vendors.BatchWindowFor(contract.ID)
		hours := map[int]bool{served.MinHours: true, served.MaxHours: true, served.MinHours - 1: false, served.MaxHours + 1: false}
		for n, admitted := range hours {
			window := strconv.Itoa(n) + "h"
			t.Run(contract.ID+" "+window, func(t *testing.T) {
				t.Parallel()
				source, err := oif.ParseJSON([]byte(`{"completion_window":"`+window+`"}`), oif.Limits{})
				if err != nil {
					t.Fatal(err)
				}
				got, refusal := batchCompletionWindow(source, contract.ID)
				if !admitted {
					if refusal == nil {
						t.Fatalf("window %s was admitted outside %+v", window, served)
					}
					return
				}
				if refusal != nil || got.Length != time.Duration(n)*time.Hour || got.Label != window {
					t.Fatalf("window = %+v, refusal = %v, want %d hours", got, refusal, n)
				}
			})
		}
	}
}
