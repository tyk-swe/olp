package media

import (
	"net/url"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/staging"
)

type VideoCreate struct {
	Prompt   string
	Seconds  *string
	Size     *string
	InputRef *staging.Part
}

func (VideoCreate) Operation() Operation { return OpVideoCreate }
func (VideoCreate) streaming() bool      { return false }
func (p VideoCreate) uploads() []staging.Part {
	if p.InputRef == nil {
		return nil
	}
	return []staging.Part{*p.InputRef}
}

func (p VideoCreate) Duration() string { return valueOr(p.Seconds, "4") }

var videoSizes = []string{"720x1280", "1280x720", "1024x1792", "1792x1024"}

func DecodeVideoCreate(form *staging.Form) (*Request, *Error) {
	route := form.Required("model")
	p := VideoCreate{Prompt: form.Required("prompt"), InputRef: form.TakeSingleFile("input_reference"), Seconds: form.Optional("seconds"), Size: form.Optional("size")}
	extra, failure := formExtensions(form)
	switch {
	case failure != nil:
		return nil, failure
	case p.Prompt == "" || len(p.Prompt) > MaxVideoPromptLength:
		return nil, invalidMedia("The video prompt must contain 1 to 32000 bytes.")
	case p.Seconds != nil && *p.Seconds != "4" && *p.Seconds != "8" && *p.Seconds != "12":
		return nil, invalidMedia("The video seconds must be one of 4, 8, or 12.")
	case p.Size != nil && !slices.Contains(videoSizes, *p.Size):
		return nil, invalidMedia("The video size is not supported.")
	case p.InputRef != nil && p.InputRef.ContentType != "" && !strings.HasPrefix(p.InputRef.ContentType, "image/"):
		return nil, invalidMedia("The video input_reference must be an image file.")
	}
	return formRequest(form, route, p, extra)
}

func encodeVideoCreate(r *Request, p VideoCreate, _ connectors.Config, model string) (*UpstreamCall, *Error) {
	fields := textValue(nil, "model", model)
	fields = textValue(fields, "prompt", p.Prompt)
	fields = textField(fields, "seconds", p.Seconds)
	fields = textField(fields, "size", p.Size)
	if p.InputRef != nil {
		fields = append(fields, staging.Field{Name: "input_reference", Value: *p.InputRef})
	}
	return &UpstreamCall{Method: "POST", Endpoint: Path{Path: "videos"}, Accept: "application/json",
		Body: FormBody(extraFields(fields, r.Extra())), Response: VideoJobReply{}, Ambiguous: true}, nil
}

type JobCall interface {
	Operation() Operation
	Upstream() string
}

type VideoGet struct{ ID string }

type VideoContent struct{ ID, Variant string }

type VideoDelete struct{ ID string }

func (VideoGet) Operation() Operation     { return OpVideoGet }
func (VideoContent) Operation() Operation { return OpVideoContent }
func (VideoDelete) Operation() Operation  { return OpVideoDelete }
func (j VideoGet) Upstream() string       { return j.ID }
func (j VideoContent) Upstream() string   { return j.ID }
func (j VideoDelete) Upstream() string    { return j.ID }

func encodeVideoGet(job VideoGet) (*UpstreamCall, *Error) {
	path, failure := VideoJobPath(job.ID, "")
	if failure != nil {
		return nil, failure
	}
	return &UpstreamCall{Method: "GET", Endpoint: Path{Path: path}, Accept: "application/json", Response: VideoJobReply{}}, nil
}

func encodeVideoDelete(job VideoDelete) (*UpstreamCall, *Error) {
	path, failure := VideoJobPath(job.ID, "")
	if failure != nil {
		return nil, failure
	}
	return &UpstreamCall{Method: "DELETE", Endpoint: Path{Path: path}, Accept: "application/json", Response: VideoDeletedReply{}, Ambiguous: true}, nil
}

func encodeVideoContent(job VideoContent) (*UpstreamCall, *Error) {
	path, failure := VideoJobPath(job.ID, "content")
	if failure != nil {
		return nil, failure
	}
	query := url.Values{}
	if job.Variant != "" {
		query.Set("variant", job.Variant)
	}
	return &UpstreamCall{Method: "GET", Endpoint: Path{Path: path, Query: query}, Accept: "*/*", Response: VideoContentReply{}}, nil
}

func (r *Request) VideoSeconds() (string, bool) {
	video, ok := r.params.(VideoCreate)
	return video.Duration(), ok
}
