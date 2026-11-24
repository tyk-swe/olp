package vendors

import "testing"

func TestForDialectNamesAVendorOnlyWhenNoOtherSpeaksTheDialect(t *testing.T) {
	t.Parallel()
	speakers := map[string][]string{}
	for _, c := range All() {
		for _, dialect := range c.Dialects {
			speakers[dialect] = append(speakers[dialect], c.ID)
		}
	}
	for dialect, ids := range speakers {
		got, ok := ForDialect(dialect)
		if len(ids) > 1 && ok {
			t.Errorf("ForDialect(%q) = %q, and %v all speak it", dialect, got, ids)
		}
		if len(ids) == 1 && (!ok || got != ids[0]) {
			t.Errorf("ForDialect(%q) = %q, %t, want its only vendor %q", dialect, got, ok, ids[0])
		}
	}
	if got, ok := ForDialect("a dialect no vendor speaks"); ok {
		t.Errorf("ForDialect named %q for a dialect no vendor speaks", got)
	}
}

func TestAVendorEitherServesBatchesThroughItsAPIOrRefusesThem(t *testing.T) {
	t.Parallel()
	for _, c := range All() {
		api, served := BatchAPIFor(c.ID)
		reason, refused := BatchRefusalFor(c.ID)
		if api != c.BatchAPI || served != (api != "") {
			t.Errorf("BatchAPIFor(%q) = %q, %t, want the contract's %q", c.ID, api, served, c.BatchAPI)
		}
		if reason != c.BatchRefusal || refused != (reason != "") {
			t.Errorf("BatchRefusalFor(%q) = %q, %t, want the contract's %q", c.ID, reason, refused, c.BatchRefusal)
		}
		if served && (refused || !c.Serves("batch")) {
			t.Errorf("%s has a batch API and does not serve batches", c.ID)
		}
		if refused && c.Serves("batch") {
			t.Errorf("%s refuses batches and lists the batch operation", c.ID)
		}
	}
	if _, served := BatchAPIFor("a vendor without a contract"); served {
		t.Error("a vendor without a contract has a batch API")
	}
	if _, refused := BatchRefusalFor("a vendor without a contract"); refused {
		t.Error("a vendor without a contract refuses batches")
	}
}
