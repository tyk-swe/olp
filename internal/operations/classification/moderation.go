package classification

import (
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operations"
	"strconv"
)

func moderationRequest(source oif.Request) (oif.View, error) {
	r := ModerationRequest{source: source, scope: Scope{Kind: "single-text", Count: 1}}
	root := source.Document().Root()
	if root.Kind() != oif.Object {
		return r, operations.Invalid("request", "Use a native moderation object.")
	}
	if model, ok := root.Lookup("model"); ok && model.Kind() != oif.String {
		return r, operations.Invalid("model", "Use a model string or omit it.")
	}
	input := operations.Member(root, "input")
	if input.Kind() == oif.String {
		return r, nil
	}
	if input.Kind() != oif.Array || len(input.Elements()) == 0 {
		return r, operations.Invalid("input", "Use native text, a text batch, or a multimodal input array.")
	}
	items := input.Elements()
	if items[0].Kind() == oif.String {
		r.scope = Scope{Kind: "text-batch", Count: len(items)}
		for _, item := range items {
			if item.Kind() != oif.String {
				return r, operations.Invalid("input", "Text batches cannot contain multimodal blocks.")
			}
		}
		return r, nil
	}
	r.scope.Kind = "joint-multimodal"
	for _, item := range items {
		if item.Kind() != oif.Object {
			return r, operations.Invalid("input", "Multimodal input entries must be objects.")
		}
		switch operations.String(operations.Member(item, "type")) {
		case "text":
			if operations.Member(item, "text").Kind() != oif.String {
				return r, operations.Invalid("input.text", "Text blocks require native text.")
			}
		case "image_url":
			if operations.Member(item, "image_url").Kind() != oif.Object || operations.String(operations.Member(operations.Member(item, "image_url"), "url")) == "" {
				return r, operations.Invalid("input.image_url", "Image blocks require a native image URL.")
			}
		default:
			return r, operations.Error("target_capability", "input.type", "native_moderation_input", "This input kind has no moderation contract.")
		}
	}
	return r, nil
}
func moderationResult(request oif.Request, source oif.Result) (oif.View, error) {
	view, err := moderationRequest(request)
	if err != nil {
		return nil, err
	}
	r := ModerationResult{source: source, scope: view.(ModerationRequest).scope}
	root := source.Source().Root()
	if root.Kind() != oif.Object || operations.Member(root, "id").Kind() != oif.String || operations.Member(root, "model").Kind() != oif.String {
		return r, operations.Violation("result", "moderation_identity")
	}
	results := operations.Member(root, "results")
	if results.Kind() != oif.Array || len(results.Elements()) != r.scope.Count {
		return r, operations.Violation("results", "moderation_result_scope")
	}
	for position, value := range results.Elements() {
		d := Decision{Position: position, Categories: operations.Member(value, "categories"), Scores: operations.Member(value, "category_scores"), Flagged: operations.Member(value, "flagged"), AppliedInputTypes: operations.Member(value, "category_applied_input_types"), Thresholds: operations.Member(value, "thresholds")}
		if value.Kind() != oif.Object || d.Flagged.Kind() != oif.Boolean || d.Categories.Kind() != oif.Object || d.Scores.Kind() != oif.Object || len(d.Categories.Members()) == 0 || len(d.Categories.Members()) != len(d.Scores.Members()) {
			return r, operations.Violation("results/"+strconv.Itoa(position), "moderation_categories")
		}
		for _, category := range d.Categories.Members() {
			score, present := d.Scores.Lookup(category.Name)
			if category.Value.Kind() != oif.Boolean || !present || score.Kind() != oif.Number {
				return r, operations.Violation("results", "moderation_category_score")
			}
		}
		if d.AppliedInputTypes.Kind() != oif.Absent {
			if d.AppliedInputTypes.Kind() != oif.Object {
				return r, operations.Violation("category_applied_input_types", "moderation_category_scope")
			}
			for _, category := range d.AppliedInputTypes.Members() {
				if _, present := d.Categories.Lookup(category.Name); !present || category.Value.Kind() != oif.Array {
					return r, operations.Violation("category_applied_input_types", "moderation_category_scope")
				}
				for _, kind := range category.Value.Elements() {
					if kind.Kind() != oif.String {
						return r, operations.Violation("category_applied_input_types", "moderation_category_scope")
					}
				}
			}
		}
		r.decisions = append(r.decisions, d)
	}
	return r, nil
}
