package media

import (
	"bytes"
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
	Extra        map[string]any
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
	Extra   map[string]any
}

func DecodeVideoObject(body []byte) (*VideoJobResult, *Error) {
	var encodedWire struct {
		ID        string   `json:"id"`
		Object    string   `json:"object"`
		Model     string   `json:"model"`
		Status    string   `json:"status"`
		Progress  *float32 `json:"progress"`
		CreatedAt *int64   `json:"created_at"`
		Completed *int64   `json:"completed_at"`
		Expires   *int64   `json:"expires_at"`
		Prompt    *string  `json:"prompt"`
		Seconds   *string  `json:"seconds"`
		Size      *string  `json:"size"`
		Error     *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, ProtocolError("The provider video response is not valid JSON.")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&encodedWire); err != nil {
		return nil, ProtocolError("The provider video response is not valid JSON.")
	}
	if encodedWire.Object != "video" {
		return nil, ProtocolError("The provider returned an unexpected video object type.")
	}
	result := &VideoJobResult{
		ID: encodedWire.ID, Status: encodedWire.Status, Progress: encodedWire.Progress,
		CreatedAt: encodedWire.CreatedAt, CompletedAt: encodedWire.Completed, ExpiresAt: encodedWire.Expires,
		Prompt: encodedWire.Prompt, Seconds: encodedWire.Seconds, Size: encodedWire.Size,
		Extra: diffFields(doc, "id", "object", "model", "status", "progress", "created_at",
			"completed_at", "expires_at", "prompt", "seconds", "size", "remixed_from_video_id", "error"),
	}
	if encodedWire.Error != nil {
		result.ErrorCode = &encodedWire.Error.Code
		result.ErrorMessage = &encodedWire.Error.Message
	}
	return result, nil
}

func EncodeVideoObject(result *VideoJobResult, localID, route string) ([]byte, *Error) {
	doc := map[string]any{
		"id":     localID,
		"object": "video",
		"model":  route,
		"status": result.Status,
	}
	if result.Progress != nil {
		doc["progress"] = *result.Progress
	}
	if result.CreatedAt != nil {
		doc["created_at"] = *result.CreatedAt
	}
	if result.CompletedAt != nil {
		doc["completed_at"] = *result.CompletedAt
	}
	if result.ExpiresAt != nil {
		doc["expires_at"] = *result.ExpiresAt
	}
	if result.Prompt != nil {
		doc["prompt"] = *result.Prompt
	}
	if result.Seconds != nil {
		doc["seconds"] = *result.Seconds
	}
	if result.Size != nil {
		doc["size"] = *result.Size
	}
	if result.ErrorMessage != nil {
		code := "video_error"
		if result.ErrorCode != nil {
			code = *result.ErrorCode
		}
		doc["error"] = map[string]any{"code": code, "message": *result.ErrorMessage}
	}
	for name, value := range result.Extra {
		if _, exists := doc[name]; !exists {
			doc[name] = value
		}
	}
	body, err := json.Marshal(doc)
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
	localJSON, _ := json.Marshal(localID)
	changes := []oif.Change{{Pointer: "/id", Value: string(localJSON), Origin: oif.ResourceBinding, Reason: "owned local video job identity"}}
	if route != "" {
		routeJSON, _ := json.Marshal(route)
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
	data := make([]any, len(result.Jobs))
	for i := range result.Jobs {
		job := result.Jobs[i]
		encoded, failure := EncodeVideoObject(&job, job.ID, job.Route)
		if failure != nil {
			return nil, failure
		}
		var item any
		if err := json.Unmarshal(encoded, &item); err != nil {
			return nil, ProtocolError("The provider video metadata could not be encoded.")
		}
		data[i] = item
	}
	doc := map[string]any{"object": "list", "data": data, "has_more": result.HasMore}
	if result.FirstID != nil {
		doc["first_id"] = *result.FirstID
	}
	if result.LastID != nil {
		doc["last_id"] = *result.LastID
	}
	body, err := json.Marshal(doc)
	if err != nil {
		return nil, ProtocolError("The provider video list could not be encoded.")
	}
	return body, nil
}

func DecodeVideoDeleteResponse(body []byte) (*VideoDeleteResult, *Error) {
	var encodedWire struct {
		ID      string  `json:"id"`
		Object  *string `json:"object"`
		Deleted bool    `json:"deleted"`
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, ProtocolError("The provider video delete response is not valid JSON.")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&encodedWire); err != nil {
		return nil, ProtocolError("The provider video delete response is not valid JSON.")
	}
	return &VideoDeleteResult{
		ID: encodedWire.ID, Deleted: encodedWire.Deleted, Object: encodedWire.Object,
		Extra: diffFields(doc, "id", "object", "deleted"),
	}, nil
}

func EncodeVideoDeleteResponse(result *VideoDeleteResult, localID string) ([]byte, *Error) {
	object := "video.deleted"
	if result.Object != nil && *result.Object != "" {
		object = *result.Object
	}
	doc := map[string]any{"id": localID, "object": object, "deleted": result.Deleted}
	for name, value := range result.Extra {
		if _, exists := doc[name]; !exists {
			doc[name] = value
		}
	}
	body, err := json.Marshal(doc)
	if err != nil {
		return nil, ProtocolError("The provider video deletion receipt could not be encoded.")
	}
	return body, nil
}
