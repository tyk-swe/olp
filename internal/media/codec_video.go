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
)

// The runway wire is Runway's task API: a video creation is a task, polled by
// its ID, whose output is a signed URL on Runway's CDN, and deleting the task
// cancels or removes it.
//
// https://docs.dev.runwayml.com/api
func init() {
	registerCodec("runway", codec{encode: map[string]func(*Request, string) (*UpstreamCall, *Error){
		OpVideoCreate:  encodeRunwayCreate,
		OpVideoGet:     encodeRunwayGet,
		OpVideoContent: encodeRunwayContent,
		OpVideoDelete:  encodeRunwayDelete,
	}})
}

// runwayTask is the path of one Runway task, whose IDs are UUIDs.
func runwayTask(id string) (string, *Error) {
	if id == "" || strings.ContainsAny(id, "/?#%") {
		return "", invalidMedia("The video job's upstream identity is malformed.")
	}
	return "tasks/" + url.PathEscape(id), nil
}

func encodeRunwayCreate(r *Request, model string) (*UpstreamCall, *Error) {
	if len(r.Extra) > 0 {
		return nil, invalidMedia("Runway does not support the requested video parameters.")
	}
	fields := map[string]any{"model": model, "promptText": r.Prompt, "ratio": "1280:720"}
	if r.Size != nil {
		var width, height int
		if _, err := fmt.Sscanf(*r.Size, "%dx%d", &width, &height); err != nil || width < 1 || height < 1 {
			return nil, invalidMedia("The size is not a Runway resolution.")
		}
		fields["ratio"] = strconv.Itoa(width) + ":" + strconv.Itoa(height)
	}
	// Runway requires a duration; OpenAI's is four seconds when unnamed.
	duration := "4"
	if r.Seconds != nil {
		duration = *r.Seconds
	}
	seconds, err := strconv.Atoi(duration)
	if err != nil || seconds < 1 {
		return nil, invalidMedia("Runway takes a whole number of seconds.")
	}
	fields["duration"] = seconds
	path := "text_to_video"
	reference := r.InputRef
	if reference != nil {
		path = "image_to_video"
		if !strings.HasPrefix(strings.ToLower(reference.ContentType), "image/") {
			return nil, invalidMedia("Runway animates an image reference only.")
		}
	}
	created := func(body []byte) (*VideoJobResult, *Error) {
		var wire struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(body, &wire) != nil || wire.ID == "" {
			return nil, protocolError("The provider created no video task.")
		}
		now := time.Now().Unix()
		return &VideoJobResult{ID: wire.ID, Status: "queued", CreatedAt: &now, Prompt: &r.Prompt, Seconds: &duration, Size: r.Size}, nil
	}
	call := &UpstreamCall{Method: http.MethodPost, Path: path, Kind: ResponseVideoJob, Ambiguous: true, DecodeVideo: created}
	call.JSONFrom = func(read func(*Part) ([]byte, *Error)) ([]byte, *Error) {
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

// runwayTaskObject is a Runway task as OpenAI's video object.
func runwayTaskObject(body []byte) (*VideoJobResult, *Error) {
	var wire struct {
		ID          string   `json:"id"`
		Status      string   `json:"status"`
		CreatedAt   string   `json:"createdAt"`
		Progress    *float32 `json:"progress"`
		Failure     *string  `json:"failure"`
		FailureCode *string  `json:"failureCode"`
	}
	if json.Unmarshal(body, &wire) != nil || wire.ID == "" {
		return nil, protocolError("The provider video task is malformed.")
	}
	statuses := map[string]string{"PENDING": "queued", "THROTTLED": "queued", "RUNNING": "in_progress", "SUCCEEDED": "completed", "FAILED": "failed", "CANCELLED": "failed"}
	status, ok := statuses[wire.Status]
	if !ok {
		return nil, protocolError("The provider video task has an unknown status.")
	}
	result := &VideoJobResult{ID: wire.ID, Status: status, ErrorCode: wire.FailureCode, ErrorMessage: wire.Failure}
	if wire.Progress != nil {
		percent := *wire.Progress * 100
		result.Progress = &percent
	}
	if created, err := time.Parse(time.RFC3339, wire.CreatedAt); err == nil {
		unix := created.Unix()
		result.CreatedAt = &unix
	}
	if wire.Status == "CANCELLED" {
		result.ErrorCode = new("cancelled")
	}
	return result, nil
}

func encodeRunwayGet(r *Request, _ string) (*UpstreamCall, *Error) {
	path, failure := runwayTask(r.JobID)
	if failure != nil {
		return nil, failure
	}
	return &UpstreamCall{Method: http.MethodGet, Path: path, Accept: "application/json", Kind: ResponseVideoJob, DecodeVideo: runwayTaskObject}, nil
}

// encodeRunwayContent reads the task and fetches its first output, which
// takes no credential.
func encodeRunwayContent(r *Request, _ string) (*UpstreamCall, *Error) {
	if r.Variant != "" && r.Variant != "video" {
		return nil, invalidMedia("Runway serves the video itself, without thumbnails or spritesheets.")
	}
	path, failure := runwayTask(r.JobID)
	if failure != nil {
		return nil, failure
	}
	return &UpstreamCall{Method: http.MethodGet, Path: path, Accept: "application/json", Kind: ResponseVideoContent,
		Next: func(body []byte) (*Step, *Error) {
			var wire struct {
				Status string   `json:"status"`
				Output []string `json:"output"`
			}
			if json.Unmarshal(body, &wire) != nil {
				return nil, protocolError("The provider video task is malformed.")
			}
			if wire.Status != "SUCCEEDED" || len(wire.Output) == 0 {
				return nil, Fail(http.StatusConflict, "video_not_ready", "The video has no content yet.")
			}
			return &Step{URL: wire.Output[0], Asset: "video/"}, nil
		}}, nil
}

func encodeRunwayDelete(r *Request, _ string) (*UpstreamCall, *Error) {
	path, failure := runwayTask(r.JobID)
	if failure != nil {
		return nil, failure
	}
	return &UpstreamCall{Method: http.MethodDelete, Path: path, Kind: ResponseVideoDelete, NoContent: true}, nil
}
