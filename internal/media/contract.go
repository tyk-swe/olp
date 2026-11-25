package media

import (
	"errors"

	"github.com/tyk-swe/olp/internal/contracts/media"
	"github.com/tyk-swe/olp/internal/platform/staging"
)

func ContractInput(request *Request, call *UpstreamCall, effective *Request) (media.Input, error) {
	owned := map[staging.Handle]bool{}
	for _, handle := range request.Uploads() {
		owned[handle] = true
	}
	parts, err := contractParts(call.fields(), owned)
	if err != nil {
		return media.Input{}, err
	}
	source, _ := request.origin.(FormSource)
	callerParts, err := contractParts(source.Parts, owned)
	if err != nil {
		return media.Input{}, err
	}
	document, _ := request.origin.(JSONSource)
	return media.Input{
		Route:       request.route,
		Mode:        effective.Mode(),
		Source:      document.Document,
		JSON:        call.json(),
		Parts:       parts,
		CallerParts: callerParts,
	}, nil
}

func contractParts(fields []staging.Field, owned map[staging.Handle]bool) ([]media.Part, error) {
	parts := make([]media.Part, len(fields))
	for i, field := range fields {
		switch value := field.Value.(type) {
		case staging.Text:
			text := string(value)
			parts[i] = media.Part{Name: field.Name, Text: &text}
		case staging.Part:
			if !owned[value.Handle] {
				return nil, errors.New("media part is not owned by this request")
			}
			blob, err := value.BlobReference()
			if err != nil {
				return nil, err
			}
			parts[i] = media.Part{Name: field.Name, Blob: blob, Filename: value.Filename, ContentType: value.ContentType}
		}
	}
	return parts, nil
}
