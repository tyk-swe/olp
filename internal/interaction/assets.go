package interaction

import (
	"net/url"
	"strconv"
	"strings"

	"github.com/tyk-swe/olp/internal/protocols/wire"

	"github.com/tyk-swe/olp/internal/oif"
)

// checkAssetResources visits dialect-owned media positions only. A tool's own
// JSON may contain identically named keys without referring to provider state.
// Resource IDs require the existing authority's ownership and serving binding;
// retaining their spelling is not authorization to dereference them.
func checkAssetResources(document oif.Document, encodedWire wire.Format, resolved map[string]string) error {
	fail := func(path string) error {
		value, _ := document.Lookup(path)
		if id, ok := value.Text(); ok && id != "" && resolved[path] == id {
			return nil
		}
		return incompatible("resource_affinity", path, "asset_resource_authority", "Provider asset references require resolved ownership and historical serving authority.")
	}
	var parts func(oif.Value, string) error
	parts = func(value oif.Value, path string) error {
		for index, part := range value.Elements() {
			at := oif.Pointer(path, strconv.Itoa(index))
			switch encodedWire {
			case wire.Chat, wire.Responses:
				if kind := valueText(member(part, "type")); kind == "file" || kind == "input_file" || kind == "input_image" || kind == "computer_screenshot" {
					if _, present := part.Lookup("file_id"); present {
						if err := fail(at + "/file_id"); err != nil {
							return err
						}
					}
					if _, present := member(part, "file").Lookup("file_id"); present {
						if err := fail(at + "/file/file_id"); err != nil {
							return err
						}
					}
				}
			case wire.Anthropic:
				if kind := valueText(member(part, "type")); kind == "image" || kind == "document" || kind == "container_upload" {
					if _, present := part.Lookup("file_id"); present {
						if err := fail(at + "/file_id"); err != nil {
							return err
						}
					}
					source := member(part, "source")
					if _, present := source.Lookup("file_id"); present || valueText(member(source, "type")) == "file" {
						if err := fail(at + "/source/file_id"); err != nil {
							return err
						}
					}
				}
				if valueText(member(part, "type")) == "tool_result" {
					if err := parts(member(part, "content"), at+"/content"); err != nil {
						return err
					}
				}
			case wire.Gemini, wire.GeminiStream:
				uri := valueText(member(member(part, "fileData"), "fileUri"))
				if uri != "" {
					parsed, err := url.Parse(uri)
					if err != nil || strings.HasPrefix(strings.TrimPrefix(uri, "/"), "files/") || strings.EqualFold(strings.TrimSuffix(parsed.Hostname(), "."), "generativelanguage.googleapis.com") || strings.EqualFold(parsed.Scheme, "gs") || strings.EqualFold(parsed.Scheme, "s3") {
						if err := fail(at + "/fileData/fileUri"); err != nil {
							return err
						}
					}
				}
			case wire.Bedrock:
				for _, kind := range []string{"image", "document", "video"} {
					if _, present := member(member(part, kind), "source").Lookup("s3Location"); present {
						if err := fail(at + "/" + kind + "/source/s3Location"); err != nil {
							return err
						}
					}
				}
				if err := parts(member(member(part, "toolResult"), "content"), at+"/toolResult/content"); err != nil {
					return err
				}
			}
		}
		return nil
	}
	root := document.Root()
	if encodedWire == wire.Responses {
		for i, tool := range member(root, "tools").Elements() {
			if valueText(member(tool, "type")) != "file_search" {
				continue
			}
			for j := range member(tool, "vector_store_ids").Elements() {
				if err := fail("/tools/" + strconv.Itoa(i) + "/vector_store_ids/" + strconv.Itoa(j)); err != nil {
					return err
				}
			}
		}
	}
	for _, field := range []string{"messages", "input", "contents"} {
		for index, message := range member(root, field).Elements() {
			path := "/" + field + "/" + strconv.Itoa(index)
			partName := "content"
			if encodedWire == wire.Responses {
				kind := valueText(member(message, "type"))
				if kind == "input_file" || kind == "input_image" || kind == "computer_screenshot" {
					if _, present := message.Lookup("file_id"); present {
						if err := fail(path + "/file_id"); err != nil {
							return err
						}
					}
				}
				if kind == "computer_call_output" || kind == "function_call_output" || kind == "custom_tool_call_output" {
					partName = "output"
					part := member(message, partName)
					if part.Kind() == oif.Object {
						partKind := valueText(member(part, "type"))
						if partKind == "input_file" || partKind == "input_image" || partKind == "computer_screenshot" {
							if _, present := part.Lookup("file_id"); present {
								if err := fail(path + "/output/file_id"); err != nil {
									return err
								}
							}
						}
					}
				} else if kind != "" && kind != "message" {
					continue
				}
			}
			if field == "contents" {
				partName = "parts"
			}
			if err := parts(member(message, partName), path+"/"+partName); err != nil {
				return err
			}
		}
	}
	return nil
}
