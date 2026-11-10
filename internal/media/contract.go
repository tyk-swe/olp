package media

import (
	"errors"

	"github.com/tyk-swe/olp/internal/mediacontract"
	"github.com/tyk-swe/olp/internal/staging"
)

func ContractInput(request *Request, call *UpstreamCall, effective *Request, budget func(int) error) (mediacontract.Input, error) {
	owned := map[staging.Handle]bool{}
	for _, handle := range request.Uploads() {
		owned[handle] = true
	}
	parts, err := contractParts(call.Fields, owned)
	if err != nil {
		return mediacontract.Input{}, err
	}
	source, _ := request.origin.(FormSource)
	callerParts, err := contractParts(source.Parts, owned)
	if err != nil {
		return mediacontract.Input{}, err
	}
	document, _ := request.origin.(JSONSource)
	return mediacontract.Input{
		Route:        request.route,
		Mode:         effective.Mode(),
		Source:       document.Document,
		JSON:         call.JSON,
		Parts:        parts,
		CallerParts:  callerParts,
		PolicyBudget: budget,
	}, nil
}

func contractParts(fields []staging.Field, owned map[staging.Handle]bool) ([]mediacontract.Part, error) {
	parts := make([]mediacontract.Part, len(fields))
	for i, field := range fields {
		switch value := field.Value.(type) {
		case staging.Text:
			text := string(value)
			parts[i] = mediacontract.Part{Name: field.Name, Text: &text}
		case staging.Part:
			if !owned[value.Handle] {
				return nil, errors.New("media part is not owned by this request")
			}
			blob, err := value.BlobReference()
			if err != nil {
				return nil, err
			}
			parts[i] = mediacontract.Part{Name: field.Name, Blob: blob, Filename: value.Filename, ContentType: value.ContentType}
		}
	}
	return parts, nil
}
