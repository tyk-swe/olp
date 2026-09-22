package interaction

import (
	"net/url"
	"strconv"
	"strings"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/openai"
)

// checkAssetResources visits dialect-owned media positions only. A tool's own
// JSON may contain identically named keys without referring to provider state.
// Resource IDs require the existing authority's ownership and serving binding;
// retaining their spelling is not authorization to dereference them.
func checkAssetResources(document oif.Document, wire openai.Family) error {
	fail := func(path string) error {
		return incompatible("resource_affinity", path, "asset_resource_authority", "Provider asset references require resolved ownership and historical serving authority.")
	}
	var parts func(oif.Value, string) error
	parts = func(value oif.Value, path string) error {
		for index, part := range value.Elements() {
			at := oif.Pointer(path, strconv.Itoa(index))
			switch wire {
			case openai.FamilyChat, openai.FamilyResponses:
				if kind := valueText(member(part, "type")); kind == "file" || kind == "input_file" || kind == "input_image" {
					if _, present := part.Lookup("file_id"); present {
						return fail(at + "/file_id")
					}
					if _, present := member(part, "file").Lookup("file_id"); present {
						return fail(at + "/file/file_id")
					}
				}
			case openai.FamilyAnthropic:
				if kind := valueText(member(part, "type")); kind == "image" || kind == "document" || kind == "container_upload" {
					if _, present := part.Lookup("file_id"); present {
						return fail(at + "/file_id")
					}
					source := member(part, "source")
					if _, present := source.Lookup("file_id"); present || valueText(member(source, "type")) == "file" {
						return fail(at + "/source/file_id")
					}
				}
				if valueText(member(part, "type")) == "tool_result" {
					if err := parts(member(part, "content"), at+"/content"); err != nil {
						return err
					}
				}
			case openai.FamilyGemini, openai.FamilyGeminiStream:
				uri := valueText(member(member(part, "fileData"), "fileUri"))
				if uri != "" {
					parsed, err := url.Parse(uri)
					if err != nil || strings.HasPrefix(uri, "files/") || parsed.Hostname() == "generativelanguage.googleapis.com" || parsed.Scheme == "gs" || parsed.Scheme == "s3" {
						return fail(at + "/fileData/fileUri")
					}
				}
			case openai.FamilyBedrock:
				for _, kind := range []string{"image", "document", "video"} {
					if _, present := member(member(part, kind), "source").Lookup("s3Location"); present {
						return fail(at + "/" + kind + "/source/s3Location")
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
	for _, field := range []string{"messages", "input", "contents"} {
		for index, message := range member(root, field).Elements() {
			path := "/" + field + "/" + strconv.Itoa(index)
			if wire == openai.FamilyResponses {
				if kind := valueText(member(message, "type")); kind != "" && kind != "message" {
					continue
				}
			}
			partName := "content"
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
