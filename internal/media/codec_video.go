package media

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/staging"
)

var runwayCodec = codec{
	videoCreate:  encodeRunwayCreate,
	videoGet:     encodeRunwayGet,
	videoContent: encodeRunwayContent,
	videoDelete:  encodeRunwayDelete,
}

func runwayTask(id string) (string, *Error) {
	if id == "" || strings.ContainsAny(id, "/?#%") {
		return "", invalidMedia("The video job's upstream identity is malformed.")
	}
	return "tasks/" + url.PathEscape(id), nil
}

func encodeRunwayCreate(r *Request, p VideoCreate, _ connectors.Config, model string) (*UpstreamCall, *Error) {
	if len(r.Extra()) > 0 {
		return nil, invalidMedia("Runway does not support the requested video parameters.")
	}
	fields := map[string]any{"model": model, "promptText": p.Prompt, "ratio": "1280:720"}
	if p.Size != nil {
		var width, height int
		if _, err := fmt.Sscanf(*p.Size, "%dx%d", &width, &height); err != nil || width < 1 || height < 1 {
			return nil, invalidMedia("The size is not a Runway resolution.")
		}
		fields["ratio"] = strconv.Itoa(width) + ":" + strconv.Itoa(height)
	}
	duration := p.Duration()
	seconds, err := strconv.Atoi(duration)
	if err != nil || seconds < 1 {
		return nil, invalidMedia("Runway takes a whole number of seconds.")
	}
	fields["duration"] = seconds
	path := "text_to_video"
	reference := p.InputRef
	if reference != nil {
		path = "image_to_video"
		if !strings.HasPrefix(strings.ToLower(reference.ContentType), "image/") {
			return nil, invalidMedia("Runway animates an image reference only.")
		}
	}
	created := func(body []byte) (*VideoJobResult, *Error) {
		var encodedWire struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(body, &encodedWire) != nil || encodedWire.ID == "" {
			return nil, protocolError("The provider created no video task.")
		}
		now := time.Now().Unix()
		return &VideoJobResult{ID: encodedWire.ID, Status: "queued", CreatedAt: &now, Prompt: &p.Prompt, Seconds: &duration, Size: p.Size}, nil
	}
	call := &UpstreamCall{Method: http.MethodPost, Path: path, Kind: ResponseVideoJob, Ambiguous: true, DecodeVideo: created}
	call.JSONFrom = func(read func(*staging.Part) ([]byte, *Error)) ([]byte, *Error) {
		if reference != nil {
			image, failure := read(reference)
			if failure != nil {
				return nil, failure
			}
			fields["promptImage"] = "data:" + reference.ContentType + ";base64," + base64.StdEncoding.EncodeToString(image)
		}
		body, err := json.Marshal(fields)
		if err != nil {
			return nil, invalidMedia("The video request could not be encoded.")
		}
		return body, nil
	}
	return call, nil
}

func runwayTaskObject(body []byte) (*VideoJobResult, *Error) {
	var encodedWire struct {
		ID          string   `json:"id"`
		Status      string   `json:"status"`
		CreatedAt   string   `json:"createdAt"`
		Progress    *float32 `json:"progress"`
		Failure     *string  `json:"failure"`
		FailureCode *string  `json:"failureCode"`
	}
	if json.Unmarshal(body, &encodedWire) != nil || encodedWire.ID == "" {
		return nil, protocolError("The provider video task is malformed.")
	}
	statuses := map[string]string{"PENDING": "queued", "THROTTLED": "queued", "RUNNING": "in_progress", "SUCCEEDED": "completed", "FAILED": "failed", "CANCELLED": "failed"}
	status, ok := statuses[encodedWire.Status]
	if !ok {
		return nil, protocolError("The provider video task has an unknown status.")
	}
	result := &VideoJobResult{ID: encodedWire.ID, Status: status, ErrorCode: encodedWire.FailureCode, ErrorMessage: encodedWire.Failure}
	if encodedWire.Progress != nil {
		percent := *encodedWire.Progress * 100
		result.Progress = &percent
	}
	if created, err := time.Parse(time.RFC3339, encodedWire.CreatedAt); err == nil {
		unix := created.Unix()
		result.CreatedAt = &unix
	}
	if encodedWire.Status == "CANCELLED" {
		result.ErrorCode = new("cancelled")
	}
	return result, nil
}

func encodeRunwayGet(job VideoGet) (*UpstreamCall, *Error) {
	path, failure := runwayTask(job.ID)
	if failure != nil {
		return nil, failure
	}
	return &UpstreamCall{Method: http.MethodGet, Path: path, Accept: "application/json", Kind: ResponseVideoJob, DecodeVideo: runwayTaskObject}, nil
}

func encodeRunwayContent(job VideoContent) (*UpstreamCall, *Error) {
	if job.Variant != "" && job.Variant != "video" {
		return nil, invalidMedia("Runway serves the video itself, without thumbnails or spritesheets.")
	}
	path, failure := runwayTask(job.ID)
	if failure != nil {
		return nil, failure
	}
	return &UpstreamCall{Method: http.MethodGet, Path: path, Accept: "application/json", Kind: ResponseVideoContent,
		Next: func(body []byte) (*Step, *Error) {
			var encodedWire struct {
				Status string   `json:"status"`
				Output []string `json:"output"`
			}
			if json.Unmarshal(body, &encodedWire) != nil {
				return nil, protocolError("The provider video task is malformed.")
			}
			if encodedWire.Status != "SUCCEEDED" || len(encodedWire.Output) == 0 {
				return nil, Fail(http.StatusConflict, "video_not_ready", "The video has no content yet.")
			}
			return &Step{URL: encodedWire.Output[0], Asset: "video/"}, nil
		}}, nil
}

func encodeRunwayDelete(job VideoDelete) (*UpstreamCall, *Error) {
	path, failure := runwayTask(job.ID)
	if failure != nil {
		return nil, failure
	}
	return &UpstreamCall{Method: http.MethodDelete, Path: path, Kind: ResponseVideoDelete, NoContent: true}, nil
}
