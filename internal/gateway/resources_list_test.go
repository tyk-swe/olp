package gateway

import (
	"encoding/json"
	"testing"
)

func TestListBodyReportsCursorPage(t *testing.T) {
	var page struct {
		Data    []json.RawMessage `json:"data"`
		HasMore bool              `json:"has_more"`
		FirstID *string           `json:"first_id"`
		LastID  *string           `json:"last_id"`
	}
	items := []json.RawMessage{json.RawMessage(`{"id":"file_a"}`), json.RawMessage(`{"id":"file_b"}`)}
	if err := json.Unmarshal(listBody(items, []string{"file_a", "file_b"}, true), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Data) != 2 || !page.HasMore || page.FirstID == nil || *page.FirstID != "file_a" || page.LastID == nil || *page.LastID != "file_b" {
		t.Fatalf("cursor page: %+v", page)
	}
	page.FirstID, page.LastID = nil, nil
	if err := json.Unmarshal(listBody([]json.RawMessage{}, nil, false), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Data) != 0 || page.HasMore || page.FirstID != nil || page.LastID != nil {
		t.Fatalf("empty page: %+v", page)
	}
}
