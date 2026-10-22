package operations

import (
	"encoding/json"
	"os"
	"testing"
)

func TestEndpointAdmissionContracts(t *testing.T) {
	data, err := os.ReadFile("../../tests/clients/m9-endpoints.json")
	if err != nil {
		t.Fatal(err)
	}
	var ledger struct {
		Endpoints []struct {
			Operation, Status string
			Contract          string `json:"admission_contract"`
		}
	}
	if err := json.Unmarshal(data, &ledger); err != nil {
		t.Fatal(err)
	}
	for _, row := range ledger.Endpoints {
		if row.Status != "pending" {
			continue
		}
		c, ok := ContractFor(row.Operation)
		if !ok || row.Contract != ContractRevision || c.Scope != "inference" || !c.InspectionByCodec {
			t.Errorf("missing admission contract for %s", row.Operation)
		}
	}
	if _, ok := ContractFor("unknown"); ok {
		t.Fatal("unknown operation acquired a contract")
	}
	if NewRegistry().HasOperation("fine_tuning") {
		t.Fatal("a declaration enabled a codec")
	}
}
