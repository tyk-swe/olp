package media

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/platform/staging"
)

var runwayCodec = codec{
	videoCreate:  encodeRunwayCreate,
	videoGet:     encodeRunwayGet,
	videoContent: encodeRunwayContent,
	videoDelete:  encodeRunwayDelete,
}

var runwayStatuses = map[string]string{
	"PENDING":   "queued",
	"THROTTLED": "queued",
	"RUNNING":   "in_progress",
	"SUCCEEDED": "completed",
	"FAILED":    "failed",
	"CANCELLED": "failed",
}

type runwayCreateRequest struct {
	Model       string `json:"model"`
	PromptText  string `json:"promptText"`
	Ratio       string `json:"ratio"`
	Duration    int    `json:"duration"`
	PromptImage string `json:"promptImage,omitempty"`
}

type runwayCreateResponse struct {
	ID string `json:"id"`
}

type runwayTaskResponse struct {
	ID          string   `json:"id"`
	Status      string   `json:"status"`
	CreatedAt   string   `json:"createdAt"`
	Progress    *float32 `json:"progress"`
	Failure     *string  `json:"failure"`
	FailureCode *string  `json:"failureCode"`
}

type runwayContentResponse struct {
	Status string   `json:"status"`
	Output []string `json:"output"`
}

func runwayTask(id string) (string, *Error) {
	if id == "" || strings.ContainsAny(id, "/?#%") {
		return "", invalidMedia("The video job's upstream identity is malformed.")
	}
	return "tasks/" + url.PathEscape(id), nil
}

func encodeRunwayCreate(r *Request, p VideoCreate, _ connectors.Config, model string) (*UpstreamCall, *Error) {
	if r.hasExtra() {
		return nil, invalidMedia("Runway does not support the requested video parameters.")
	}
	ratio := "1280:720"
	if p.Size != nil {
		width, height, ok := parseDimensions(*p.Size)
		if !ok || width < 1 || height < 1 {
			return nil, invalidMedia("The size is not a Runway resolution.")
		}
		ratio = strconv.Itoa(width) + ":" + strconv.Itoa(height)
	}
	duration := p.Duration()
	seconds, err := strconv.Atoi(duration)
	if err != nil || seconds < 1 {
		return nil, invalidMedia("Runway takes a whole number of seconds.")
	}
	path := "text_to_video"
	if p.InputRef != nil {
		path = "image_to_video"
		if !strings.HasPrefix(strings.ToLower(p.InputRef.ContentType), "image/") {
			return nil, invalidMedia("Runway animates an image reference only.")
		}
	}
	baseReq := runwayCreateRequest{Model: model, PromptText: p.Prompt, Ratio: ratio, Duration: seconds}
	return &UpstreamCall{
		Method:    http.MethodPost,
		Endpoint:  Path{Path: path},
		Body:      runwayCreateBody(baseReq, p.InputRef),
		Response:  VideoJobReply{Decode: decodeRunwayCreate(p.Prompt, duration, p.Size)},
		Ambiguous: true,
	}, nil
}

func runwayCreateBody(base runwayCreateRequest, reference *staging.Part) InlineBody {
	return func(read func(staging.Part) ([]byte, *Error)) ([]byte, *Error) {
		payload := base
		if reference != nil {
			image, failure := read(*reference)
			if failure != nil {
				return nil, failure
			}
			payload.PromptImage = "data:" + reference.ContentType + ";base64," + base64.StdEncoding.EncodeToString(image)
		}
		body, err := json.Marshal(payload)
		if err != nil {
			return nil, invalidMedia("The video request could not be encoded.")
		}
		return body, nil
	}
}

func decodeRunwayCreate(prompt, duration string, size *string) func([]byte) (*VideoJobResult, *Error) {
	return func(body []byte) (*VideoJobResult, *Error) {
		var resp runwayCreateResponse
		if json.Unmarshal(body, &resp) != nil || resp.ID == "" {
			return nil, ProtocolError("The provider created no video task.")
		}
		now := nowUnix()
		promptCopy := prompt
		durationCopy := duration
		return &VideoJobResult{ID: resp.ID, Status: "queued", CreatedAt: &now, Prompt: &promptCopy, Seconds: &durationCopy, Size: size}, nil
	}
}

func runwayTaskObject(body []byte) (*VideoJobResult, *Error) {
	var resp runwayTaskResponse
	if json.Unmarshal(body, &resp) != nil || resp.ID == "" {
		return nil, ProtocolError("The provider video task is malformed.")
	}
	status, ok := runwayStatuses[resp.Status]
	if !ok {
		return nil, ProtocolError("The provider video task has an unknown status.")
	}
	result := &VideoJobResult{ID: resp.ID, Status: status, ErrorCode: resp.FailureCode, ErrorMessage: resp.Failure}
	if resp.Progress != nil {
		percent := *resp.Progress * 100
		result.Progress = &percent
	}
	if created, err := time.Parse(time.RFC3339, resp.CreatedAt); err == nil {
		unix := created.Unix()
		result.CreatedAt = &unix
	}
	if resp.Status == "CANCELLED" {
		result.ErrorCode = new("cancelled")
	}
	return result, nil
}

func encodeRunwayGet(job VideoGet) (*UpstreamCall, *Error) {
	path, failure := runwayTask(job.ID)
	if failure != nil {
		return nil, failure
	}
	return &UpstreamCall{Method: http.MethodGet, Endpoint: Path{Path: path}, Accept: "application/json", Response: job.reply(runwayTaskObject)}, nil
}

func encodeRunwayContent(job VideoContent) (*UpstreamCall, *Error) {
	if job.Variant != "" && job.Variant != "video" {
		return nil, invalidMedia("Runway serves the video itself, without thumbnails or spritesheets.")
	}
	path, failure := runwayTask(job.ID)
	if failure != nil {
		return nil, failure
	}
	return &UpstreamCall{
		Method:   http.MethodGet,
		Endpoint: Path{Path: path},
		Accept:   "application/json",
		Response: VideoContentReply{Poll: &Poll{Next: nextRunwayContentStep}},
	}, nil
}

func nextRunwayContentStep(body []byte) (Step, *Error) {
	var resp runwayContentResponse
	if json.Unmarshal(body, &resp) != nil {
		return nil, ProtocolError("The provider video task is malformed.")
	}
	if resp.Status != "SUCCEEDED" || len(resp.Output) == 0 {
		return nil, Fail(http.StatusConflict, "video_not_ready", "The video has no content yet.")
	}
	return Fetch{URL: resp.Output[0], Prefix: "video/"}, nil
}

func encodeRunwayDelete(job VideoDelete) (*UpstreamCall, *Error) {
	path, failure := runwayTask(job.ID)
	if failure != nil {
		return nil, failure
	}
	return &UpstreamCall{Method: http.MethodDelete, Endpoint: Path{Path: path}, Response: VideoDeletedReply{NoContent: true}}, nil
}
