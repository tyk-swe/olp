package search

import (
	"strconv"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/operations"
)

// outputText covers each dialect's result text: title, url, snippet and the
// content or excerpt fields. Reviewed metadata fields pass without inspection;
// anything the contract does not name fails closed unless it carries no text.
func outputText(result oif.Result, id string) ([]operations.Text, error) {
	if id == Brave {
		return braveOutputText(result)
	}
	out := []operations.Text{}
	known := map[string]bool{"id": true, "results": true, "server_time": true}
	switch id {
	case Tavily:
		known["query"], known["response_time"], known["request_id"] = true, true, true
	case Exa:
		known["requestId"], known["costDollars"], known["searchTime"] = true, true, true
	case Parallel:
		known["search_id"], known["session_id"], known["usage"], known["warnings"] = true, true, true, true
	}
	root := result.Source().Root()
	for _, f := range root.Members() {
		if !known[f.Name] && !inert(f.Value) {
			return nil, conflict(operations.Pointer("", f.Name))
		}
	}
	inspect := map[string]bool{"title": true, "url": true, "snippet": true, "content": true, "author": true, "text": true, "summary": true}
	metadata := map[string]bool{"id": true, "score": true, "date": true, "last_updated": true, "publishedDate": true, "publish_date": true, "favicon": true, "image": true, "highlightScores": true}
	for i, row := range member(root, "results").Elements() {
		path := "/results/" + strconv.Itoa(i)
		for _, f := range row.Members() {
			p := operations.Pointer(path, f.Name)
			switch {
			case inspect[f.Name]:
				if f.Value.Kind() == oif.String {
					out = append(out, operations.Text{Pointer: p, Value: text(f.Value)})
				} else if !inert(f.Value) {
					return nil, conflict(p)
				}
			case f.Name == "highlights" || f.Name == "excerpts":
				if f.Value.Kind() != oif.Array && !inert(f.Value) {
					return nil, conflict(p)
				}
				for j, e := range f.Value.Elements() {
					if e.Kind() == oif.String {
						out = append(out, operations.Text{Pointer: p + "/" + strconv.Itoa(j), Value: text(e)})
					} else if !inert(e) {
						return nil, conflict(p + "/" + strconv.Itoa(j))
					}
				}
			case metadata[f.Name]:
			default:
				if !inert(f.Value) {
					return nil, conflict(p)
				}
			}
		}
	}
	return out, nil
}

func braveOutputText(result oif.Result) ([]operations.Text, error) {
	out := []operations.Text{}
	root := result.Source().Root()
	for _, f := range root.Members() {
		switch f.Name {
		case "type":
		case "query":
			collectStrings(f.Value, operations.Pointer("", f.Name), &out)
		case "web":
			if err := braveWebText(f.Value, &out); err != nil {
				return nil, err
			}
		default:
			if !inert(f.Value) {
				return nil, conflict(operations.Pointer("", f.Name))
			}
		}
	}
	return out, nil
}

func braveWebText(web oif.Value, out *[]operations.Text) error {
	for _, f := range web.Members() {
		switch f.Name {
		case "type", "family_friendly":
		case "results":
			for i, row := range f.Value.Elements() {
				if err := braveRowText(row, "/web/results/"+strconv.Itoa(i), out); err != nil {
					return err
				}
			}
		default:
			if !inert(f.Value) {
				return conflict(operations.Pointer("/web", f.Name))
			}
		}
	}
	return nil
}

func braveRowText(row oif.Value, path string, out *[]operations.Text) error {
	inspect := map[string]bool{"title": true, "url": true, "description": true}
	containers := map[string]bool{"profile": true, "meta_url": true, "thumbnail": true}
	metadata := map[string]bool{"is_source_local": true, "is_source_both": true, "page_age": true, "page_fetched": true, "fetched_content_timestamp": true, "language": true, "family_friendly": true, "type": true, "subtype": true, "is_live": true, "age": true}
	for _, f := range row.Members() {
		p := operations.Pointer(path, f.Name)
		switch {
		case inspect[f.Name]:
			if f.Value.Kind() == oif.String {
				*out = append(*out, operations.Text{Pointer: p, Value: text(f.Value)})
			} else if !inert(f.Value) {
				return conflict(p)
			}
		case f.Name == "extra_snippets":
			if f.Value.Kind() != oif.Array && !inert(f.Value) {
				return conflict(p)
			}
			for j, e := range f.Value.Elements() {
				if e.Kind() == oif.String {
					*out = append(*out, operations.Text{Pointer: p + "/" + strconv.Itoa(j), Value: text(e)})
				} else if !inert(e) {
					return conflict(p + "/" + strconv.Itoa(j))
				}
			}
		case containers[f.Name]:
			collectStrings(f.Value, p, out)
		case metadata[f.Name]:
		default:
			if !inert(f.Value) {
				return conflict(p)
			}
		}
	}
	return nil
}

func collectStrings(v oif.Value, pointer string, out *[]operations.Text) {
	switch v.Kind() {
	case oif.String:
		*out = append(*out, operations.Text{Pointer: pointer, Value: text(v)})
	case oif.Array:
		for i, e := range v.Elements() {
			collectStrings(e, pointer+"/"+strconv.Itoa(i), out)
		}
	case oif.Object:
		for _, m := range v.Members() {
			collectStrings(m.Value, operations.Pointer(pointer, m.Name), out)
		}
	}
}

// inert reports whether a field carries no text the output policy could miss:
// null, scalar and empty-container values only.
func inert(v oif.Value) bool {
	switch v.Kind() {
	case oif.Absent, oif.Null, oif.Number, oif.Boolean:
		return true
	case oif.Array:
		return len(v.Elements()) == 0
	case oif.Object:
		return len(v.Members()) == 0
	}
	return false
}
