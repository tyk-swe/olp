package kernel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"

	"github.com/tyk-swe/olp/internal/domain/protocols/openai"
	"github.com/tyk-swe/olp/internal/platform/panics"
	"github.com/tyk-swe/olp/internal/staging"
)

func (s *Server) UploadMultipart(ctx context.Context, x *Call, p *Pin, endpoint, purpose string, extra map[string]string, file *staging.Part, fileFirst bool) (*PinnedResponse, *AttemptFailure) {
	pipeR, pipeW := io.Pipe()
	form := multipart.NewWriter(pipeW)
	sendDone := make(chan error, 1)
	go func() {
		failure := func(err error) error {
			pipeW.CloseWithError(err)
			return err
		}
		defer panics.Recover(s.Log, "multipart upload", func(err error) { sendDone <- failure(err) })
		writeFile := func() error {
			opened, err := s.Deps.Staging.Open(file.Handle)
			if err != nil {
				return fmt.Errorf("%w: %w", errPinnedUploadRead, err)
			}
			defer opened.File.Close()
			name := opened.Filename
			if name == "" {
				name = "upload"
			}
			header := textproto.MIMEHeader{}
			header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, stateQuote(name)))
			contentType := opened.Artifact.ContentType
			if contentType == "" {
				contentType = "application/octet-stream"
			}
			header.Set("Content-Type", contentType)
			part, err := form.CreatePart(header)
			if err != nil {
				return err
			}
			_, err = io.Copy(part, PinnedUploadSource{opened.File})
			return err
		}
		if purpose != "" && fileFirst {
			if err := writeFile(); err != nil {
				sendDone <- failure(err)
				return
			}
		}
		if purpose != "" {
			if err := form.WriteField("purpose", purpose); err != nil {
				sendDone <- failure(err)
				return
			}
		}
		for name, value := range extra {
			if err := form.WriteField(name, value); err != nil {
				sendDone <- failure(err)
				return
			}
		}
		if purpose == "" || !fileFirst {
			if err := writeFile(); err != nil {
				sendDone <- failure(err)
				return
			}
		}
		if err := form.Close(); err != nil {
			sendDone <- failure(err)
			return
		}
		sendDone <- pipeW.Close()
	}()
	joined := false
	defer func() {
		_ = pipeR.CloseWithError(io.ErrClosedPipe)
		if !joined {
			<-sendDone
		}
	}()
	exchange := s.beginPinned(ctx, x, p, http.MethodPost)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, pipeR)
	if err != nil {
		return nil, exchange.fail(ClassConnect, nil)
	}
	req.Header.Set("Content-Type", form.FormDataContentType())
	req.Header.Set("User-Agent", "olp/gateway")
	req.Header.Set("Accept", "application/json")
	if err := s.applySlotCredential(ctx, x, req, p.Provider.Connector(), p.Slot, nil, &p.Provider, p.Model); err != nil {
		return nil, exchange.fail(classCredential, nil)
	}
	maps.Copy(req.Header, x.PinnedHeaders)
	resp, failure := s.doPinnedRequest(ctx, p, exchange, req, openai.ParseErrorBody)
	// A provider can respond before consuming the body. Keep its rejection as
	// authoritative; only successful headers with an incomplete upload are invalid.
	_ = pipeR.CloseWithError(io.ErrClosedPipe)
	sendErr := <-sendDone
	joined = true
	if failure != nil {
		if failure.Status == 0 && errors.Is(sendErr, errPinnedUploadRead) {
			exchange.fact().LocalRefusal = true
		}
		return nil, failure
	}
	if sendErr != nil {
		exchange.fact().LocalRefusal = errors.Is(sendErr, errPinnedUploadRead)
		_ = resp.Body.Close()
		return nil, exchange.fail(exchange.state.classify(sendErr, false), nil)
	}
	return resp, nil
}

var stateQuoteReplacer = strings.NewReplacer("\\", "\\\\", `"`, "\\\"", "\r", "", "\n", "", "\x00", "").Replace

func stateQuote(value string) string { return stateQuoteReplacer(value) }
