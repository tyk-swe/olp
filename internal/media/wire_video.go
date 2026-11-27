package media

import (
	"encoding/json"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
)

type VideoJobResult struct {
	ID           string
	Route        string
	Status       string
	Progress     *float32
	CreatedAt    *int64
	CompletedAt  *int64
	ExpiresAt    *int64
	Prompt       *string
	Seconds      *string
	Size         *string
	ErrorCode    *string
	ErrorMessage *string
	Extra        map[string]json.RawMessage
}

type VideoListResult struct {
	Jobs    []VideoJobResult
	FirstID *string
	LastID  *string
	HasMore bool
}

type VideoDeleteResult struct {
	ID      string
	Deleted bool
	Object  *string
	Extra   map[string]json.RawMessage
}

type videoErrorWire struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type videoObjectWire struct {
	ID          string          `json:"id"`
	Object      string          `json:"object"`
	Model       string          `json:"model"`
	Status      string          `json:"status"`
	Progress    *float32        `json:"progress,omitempty"`
	CreatedAt   *int64          `json:"created_at,omitempty"`
	CompletedAt *int64          `json:"completed_at,omitempty"`
	ExpiresAt   *int64          `json:"expires_at,omitempty"`
	Prompt      *string         `json:"prompt,omitempty"`
	Seconds     *string         `json:"seconds,omitempty"`
	Size        *string         `json:"size,omitempty"`
	Error       *videoErrorWire `json:"error,omitempty"`
}

type videoListWire struct {
	Object  string            `json:"object"`
	Data    []json.RawMessage `json:"data"`
	HasMore bool              `json:"has_more"`
	FirstID *string           `json:"first_id,omitempty"`
	LastID  *string           `json:"last_id,omitempty"`
}

type videoDeleteWire struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Deleted bool   `json:"deleted"`
}

func DecodeVideoObject(body []byte) (*VideoJobResult, *Error) {
	raw, err := parseRawObject(body)
	if err != nil {
		return nil, ProtocolError("The provider video response is not valid JSON.")
	}
	var wireObj videoObjectWire
	if firstErr(
		decodeField(raw, "id", &wireObj.ID),
		decodeField(raw, "object", &wireObj.Object),
		decodeField(raw, "model", &wireObj.Model),
		decodeField(raw, "status", &wireObj.Status),
		decodeField(raw, "progress", &wireObj.Progress),
		decodeField(raw, "created_at", &wireObj.CreatedAt),
		decodeField(raw, "completed_at", &wireObj.CompletedAt),
		decodeField(raw, "expires_at", &wireObj.ExpiresAt),
		decodeField(raw, "prompt", &wireObj.Prompt),
		decodeField(raw, "seconds", &wireObj.Seconds),
		decodeField(raw, "size", &wireObj.Size),
		decodeField(raw, "error", &wireObj.Error),
	) != nil {
		return nil, ProtocolError("The provider video response is not valid JSON.")
	}
	if wireObj.Object != "video" {
		return nil, ProtocolError("The provider returned an unexpected video object type.")
	}
	result := &VideoJobResult{
		ID:          wireObj.ID,
		Status:      wireObj.Status,
		Progress:    wireObj.Progress,
		CreatedAt:   wireObj.CreatedAt,
		CompletedAt: wireObj.CompletedAt,
		ExpiresAt:   wireObj.ExpiresAt,
		Prompt:      wireObj.Prompt,
		Seconds:     wireObj.Seconds,
		Size:        wireObj.Size,
		Extra: diffRawFields(raw, "id", "object", "model", "status", "progress", "created_at",
			"completed_at", "expires_at", "prompt", "seconds", "size", "remixed_from_video_id", "error"),
	}
	if wireObj.Error != nil {
		result.ErrorCode = &wireObj.Error.Code
		result.ErrorMessage = &wireObj.Error.Message
	}
	return result, nil
}

func EncodeVideoObject(result *VideoJobResult, localID, route string) ([]byte, *Error) {
	doc := videoObjectWire{
		ID:          localID,
		Object:      "video",
		Model:       route,
		Status:      result.Status,
		Progress:    result.Progress,
		CreatedAt:   result.CreatedAt,
		CompletedAt: result.CompletedAt,
		ExpiresAt:   result.ExpiresAt,
		Prompt:      result.Prompt,
		Seconds:     result.Seconds,
		Size:        result.Size,
	}
	if result.ErrorMessage != nil {
		code := "video_error"
		if result.ErrorCode != nil {
			code = *result.ErrorCode
		}
		doc.Error = &videoErrorWire{Code: code, Message: *result.ErrorMessage}
	}
	body, err := marshalWithExtra(doc, result.Extra)
	if err != nil {
		return nil, ProtocolError("The provider video metadata could not be encoded.")
	}
	return body, nil
}

func EncodeStrictVideoObject(source oif.Document, upstreamID, localID, route string) ([]byte, *Error) {
	if !source.Valid() || source.Root().Kind() != oif.Object {
		return nil, ProtocolError("The native video metadata is unavailable.")
	}
	id, ok := source.Root().Lookup("id")
	actual, valid := id.Text()
	if !ok || !valid || actual != upstreamID {
		return nil, ProtocolError("The native video identity changed.")
	}
	localJSON, err := json.Marshal(localID)
	if err != nil {
		return nil, ProtocolError("The native video metadata could not be projected.")
	}
	changes := []oif.Change{{Pointer: "/id", Value: string(localJSON), Origin: oif.ResourceBinding, Reason: "owned local video job identity"}}
	if route != "" {
		routeJSON, err := json.Marshal(route)
		if err != nil {
			return nil, ProtocolError("The native video metadata could not be projected.")
		}
		changes = append(changes, oif.Change{Pointer: "/model", Value: string(routeJSON), Origin: oif.IdentityBinding, Reason: "published route identity"})
	}
	client, err := oif.Apply(source, changes)
	if err != nil {
		return nil, ProtocolError("The native video metadata could not be projected.")
	}
	return client.Bytes(), nil
}

func ValidStrictVideoJobSource(source oif.Document, upstreamID, model string) bool {
	if !source.Valid() || source.Root().Kind() != oif.Object {
		return false
	}
	id, idOK := source.Root().Lookup("id")
	modelValue, modelOK := source.Root().Lookup("model")
	object, objectOK := source.Root().Lookup("object")
	status, statusOK := source.Root().Lookup("status")
	gotID, idText := id.Text()
	gotModel, modelText := modelValue.Text()
	gotObject, objectText := object.Text()
	gotStatus, statusText := status.Text()
	_, validStatus := wire.VideoJobStateOf(gotStatus)
	return idOK && idText && gotID == upstreamID && modelOK && modelText && gotModel == model &&
		objectOK && objectText && gotObject == "video" && statusOK && statusText &&
		validStatus
}

func EncodeVideoListResponse(result *VideoListResult) ([]byte, *Error) {
	data := make([]json.RawMessage, len(result.Jobs))
	for i := range result.Jobs {
		job := result.Jobs[i]
		encoded, failure := EncodeVideoObject(&job, job.ID, job.Route)
		if failure != nil {
			return nil, failure
		}
		data[i] = json.RawMessage(encoded)
	}
	doc := videoListWire{
		Object:  "list",
		Data:    data,
		HasMore: result.HasMore,
		FirstID: result.FirstID,
		LastID:  result.LastID,
	}
	body, err := json.Marshal(doc)
	if err != nil {
		return nil, ProtocolError("The provider video list could not be encoded.")
	}
	return body, nil
}

func DecodeVideoDeleteResponse(body []byte) (*VideoDeleteResult, *Error) {
	raw, err := parseRawObject(body)
	if err != nil {
		return nil, ProtocolError("The provider video delete response is not valid JSON.")
	}
	var id string
	var object *string
	var deleted bool
	if firstErr(
		decodeField(raw, "id", &id),
		decodeField(raw, "object", &object),
		decodeField(raw, "deleted", &deleted),
	) != nil {
		return nil, ProtocolError("The provider video delete response is not valid JSON.")
	}
	return &VideoDeleteResult{
		ID:      id,
		Deleted: deleted,
		Object:  object,
		Extra:   diffRawFields(raw, "id", "object", "deleted"),
	}, nil
}

func EncodeVideoDeleteResponse(result *VideoDeleteResult, localID string) ([]byte, *Error) {
	object := "video.deleted"
	if result.Object != nil && *result.Object != "" {
		object = *result.Object
	}
	doc := videoDeleteWire{
		ID:      localID,
		Object:  object,
		Deleted: result.Deleted,
	}
	body, err := marshalWithExtra(doc, result.Extra)
	if err != nil {
		return nil, ProtocolError("The provider video deletion receipt could not be encoded.")
	}
	return body, nil
}
