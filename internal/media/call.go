package media

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime/multipart"
	"net/textproto"
	"net/url"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/staging"
)

type UpstreamCall struct {
	Method    string
	Endpoint  Endpoint
	Accept    string
	Body      Body
	Response  Response
	Ambiguous bool
}

type Endpoint interface{ endpoint() }

type Path struct {
	Path  string
	Query url.Values
}

type Absolute struct {
	URL            string
	SigningService string
}

func (Path) endpoint()     {}
func (Absolute) endpoint() {}

func Resolve(endpoint Endpoint, cfg connectors.Config, model string) (string, connectors.Config, error) {
	switch e := endpoint.(type) {
	case Path:
		resolved, err := cfg.MediaURL(e.Path, model, e.Query)
		return resolved, cfg, err
	case Absolute:
		signed := cfg
		signed.SigningService = e.SigningService
		return e.URL, signed, nil
	}
	return "", cfg, errors.New("media endpoint is not set")
}

type Body interface{ body() }

type JSONBody []byte

type NativeJSON []byte

type FormBody []staging.Field

type UploadBody staging.Part

type InlineBody func(read func(staging.Part) ([]byte, *Error)) ([]byte, *Error)

func (JSONBody) body()   {}
func (NativeJSON) body() {}
func (FormBody) body()   {}
func (UploadBody) body() {}
func (InlineBody) body() {}

func (c *UpstreamCall) withBody(body Body) *UpstreamCall {
	next := *c
	next.Body = body
	return &next
}

func (c *UpstreamCall) withResponse(response Response) *UpstreamCall {
	next := *c
	next.Response = response
	return &next
}

func (c *UpstreamCall) json() []byte {
	switch body := c.Body.(type) {
	case JSONBody:
		return body
	case NativeJSON:
		return body
	}
	return nil
}

func (c *UpstreamCall) fields() []staging.Field {
	fields, _ := c.Body.(FormBody)
	return fields
}

func (c *UpstreamCall) Streaming() bool {
	_, events := c.Response.(EventReply)
	return events
}

type OpenPart func(staging.Part) (io.ReadCloser, staging.Part, error)

func WriteForm(form *multipart.Writer, fields []staging.Field, open OpenPart) error {
	for _, field := range fields {
		if err := writeField(form, field, open); err != nil {
			return err
		}
	}
	return form.Close()
}

func writeField(form *multipart.Writer, field staging.Field, open OpenPart) error {
	switch value := field.Value.(type) {
	case staging.Text:
		return form.WriteField(field.Name, string(value))
	case staging.Part:
		reader, part, err := open(value)
		if err != nil {
			return fmt.Errorf("open %s: %w", field.Name, err)
		}
		defer reader.Close()
		writer, err := form.CreatePart(partHeader(field.Name, part))
		if err != nil {
			return err
		}
		_, err = io.Copy(writer, reader)
		return err
	}
	return fmt.Errorf("field %s has no value", field.Name)
}

func partHeader(field string, part staging.Part) textproto.MIMEHeader {
	return textproto.MIMEHeader{
		"Content-Disposition": {fmt.Sprintf(`form-data; name="%s"; filename="%s"`, quoteEscaper(field), quoteEscaper(part.Filename))},
		"Content-Type":        {cmp.Or(part.ContentType, "application/octet-stream")},
	}
}

var quoteEscaper = strings.NewReplacer("\\", "\\\\", `"`, "\\\"", "\r", "", "\n", "", "\x00", "").Replace

var deliveryNames = []string{"model", "stream", "stream_format"}

func OutboundParameterNames(call *UpstreamCall, request *Request) ([]string, error) {
	switch body := call.Body.(type) {
	case JSONBody:
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
			return nil, errors.New("configured media body is not a JSON object")
		}
		return parameterNames(slices.Collect(maps.Keys(fields))), nil
	case FormBody:
		names := make([]string, 0, len(body))
		for _, field := range body {
			if _, file := field.Value.(staging.Part); !file || field.Name == "mask" || field.Name == "input_reference" {
				names = append(names, field.Name)
			}
		}
		return parameterNames(names), nil
	}
	return request.ParameterNames(), nil
}

func parameterNames(names []string) []string {
	kept := map[string]struct{}{}
	for _, name := range names {
		trimmed := strings.TrimSuffix(name, "[]")
		if trimmed != "" && !slices.Contains(deliveryNames, trimmed) && !strings.HasPrefix(trimmed, "__olp_") {
			kept[trimmed] = struct{}{}
		}
	}
	return slices.Sorted(maps.Keys(kept))
}
