package agents

import (
	"bytes"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/secrets"
)

func TestApprovalBindingIsExactAndDomainSeparated(t *testing.T) {
	id := "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607"
	key := secrets.NewAuthKey(bytes.Repeat([]byte{1}, 32), id)
	b := ApprovalBinding{RunID: id, ToolsetID: id, ToolsetRevisionID: id, ToolAlias: "t_" + strings.Repeat("a", 32) + "_" + strings.Repeat("b", 32), PrincipalDigest: strings.Repeat("a", 64), ArgumentDigest: strings.Repeat("b", 64), TranscriptDigest: strings.Repeat("c", 64), MaxCost: "1.25", Currency: "USD"}
	want, err := approvalDigest(key, b)
	if err != nil {
		t.Fatal(err)
	}
	changes := []func(*ApprovalBinding){func(v *ApprovalBinding) { v.ArgumentDigest = strings.Repeat("d", 64) }, func(v *ApprovalBinding) { v.TranscriptDigest = strings.Repeat("d", 64) }, func(v *ApprovalBinding) { v.PrincipalDigest = strings.Repeat("d", 64) }, func(v *ApprovalBinding) { v.RouteRevisionID = id }, func(v *ApprovalBinding) { v.MaxCost = "1.26" }, func(v *ApprovalBinding) { v.Currency = "EUR" }}
	for _, change := range changes {
		copy := b
		change(&copy)
		got, err := approvalDigest(key, copy)
		if err != nil || bytes.Equal(want, got) {
			t.Fatal("changed approval reused binding", err)
		}
	}
	if bytes.Equal(want, key.Digest(secrets.MutationDigest, "anything")) {
		t.Fatal("approval digest lacks purpose separation")
	}
	b.ArgumentDigest = "private argument body"
	if _, err := approvalDigest(key, b); err == nil {
		t.Fatal("accepted runtime content as metadata digest")
	}
}
