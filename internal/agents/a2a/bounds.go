package a2a

import (
	"bytes"
	"errors"
	"io"
	"mime"
	"net/http"

	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2acompat/a2av0"
	"github.com/tyk-swe/olp/internal/agents"
)

type limitedTransport struct{ Base http.RoundTripper }

func (t limitedTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.Base.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	mediaType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if mediaType == "text/event-stream" {
		return limitedResponse(response, &boundedEventBody{ReadCloser: response.Body}, response.ContentLength), nil
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxRPCBytes+1))
	if err != nil || len(data) > maxRPCBytes {
		return nil, errors.New("upstream response size")
	}
	return limitedResponse(response, io.NopCloser(bytes.NewReader(data)), int64(len(data))), nil
}

func limitedResponse(response *http.Response, body io.ReadCloser, length int64) *http.Response {
	return &http.Response{Status: response.Status, StatusCode: response.StatusCode, Proto: response.Proto, ProtoMajor: response.ProtoMajor, ProtoMinor: response.ProtoMinor, Header: response.Header, Body: body, ContentLength: length, TransferEncoding: response.TransferEncoding, Close: response.Close, Uncompressed: response.Uncompressed, Trailer: response.Trailer, Request: response.Request, TLS: response.TLS}
}

func upstreamTransport(version, endpoint string, client *http.Client) a2aclient.Transport {
	if version == legacyProtocol {
		return a2av0.NewJSONRPCTransport(a2av0.JSONRPCTransportConfig{URL: endpoint, Client: client})
	}
	return a2aclient.NewJSONRPCTransport(endpoint, client)
}

// Event framing must persist across reads because network chunks can split SSE lines.
type boundedEventBody struct {
	io.ReadCloser
	size    int
	hasLine bool
}

func (b *boundedEventBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	for i, value := range p[:n] {
		b.size++
		if b.size > maxRPCBytes {
			return i, agents.ErrStreamSize
		}
		switch value {
		case '\n':
			if !b.hasLine {
				b.size = 0
			}
			b.hasLine = false
		case '\r':
		default:
			b.hasLine = true
		}
	}
	return n, err
}
