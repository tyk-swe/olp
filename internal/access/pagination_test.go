package access

import (
	"encoding/base64"
	"encoding/json"
	"slices"
	"testing"
)

func TestListRepliesPreservePaginationAndEmptyArrays(t *testing.T) {
	type item struct {
		ID      string `json:"id"`
		OrderID string `json:"order_id"`
	}
	items := []item{{"first", "order-first"}, {"second", "order-second"}, {"extra", "order-extra"}}
	for _, tc := range []struct {
		name  string
		items []item
		ids   []string
		next  string
	}{
		{name: "nil"},
		{name: "empty", items: []item{}},
		{name: "short", items: items[:1], ids: []string{"first"}},
		{name: "exact limit", items: items[:2], ids: []string{"first", "second"}},
		{name: "overflow", items: items, ids: []string{"first", "second"}, next: "second"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var maps []map[string]any
			for _, item := range tc.items {
				maps = append(maps, map[string]any{"id": item.ID, "order_id": item.OrderID})
			}
			page := Pagination{Limit: 2}
			for _, shape := range []struct {
				name  string
				reply Reply
				order bool
			}{
				{"maps", ListReply(maps, page), false},
				{"maps with ordering", ListReplyBy(maps, page, func(v map[string]any) string { return v["order_id"].(string) }), true},
				{"typed records", ListReplyBy(tc.items, page, func(v item) string { return v.OrderID }), true},
			} {
				t.Run(shape.name, func(t *testing.T) {
					data, err := json.Marshal(shape.reply.Body)
					if err != nil {
						t.Fatal(err)
					}
					var response struct {
						Items      []item  `json:"items"`
						NextCursor *string `json:"next_cursor"`
					}
					if err := json.Unmarshal(data, &response); err != nil {
						t.Fatal(err)
					}
					if response.Items == nil {
						t.Fatalf("items must be an array: %s", data)
					}
					var ids []string
					for _, item := range response.Items {
						ids = append(ids, item.ID)
					}
					if !slices.Equal(ids, tc.ids) {
						t.Errorf("items = %v, want %v", ids, tc.ids)
					}
					if tc.next == "" {
						if response.NextCursor != nil {
							t.Errorf("unexpected next cursor: %s", *response.NextCursor)
						}
						return
					}
					want := tc.next
					if shape.order {
						want = "order-" + want
					}
					if response.NextCursor == nil || *response.NextCursor != base64.RawURLEncoding.EncodeToString([]byte(want)) {
						t.Errorf("cursor does not identify the last returned item: %s", data)
					}
				})
			}
		})
	}
}
