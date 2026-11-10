package protocols

import (
	"io"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/wire"
)

type ResponseCodec struct {
	native     *wire.Request
	caller     *wire.Request
	route      string
	encoding   string
	candidates int64
}

func PrepareResponse(native, caller *wire.Request, route, encoding string) ResponseCodec {
	candidates := int64(1)
	if native.Family == wire.Gemini {
		value, ok := native.OIF().Document().Lookup("/generationConfig/candidateCount")
		if ok {
			if count, valid := count(value.Bytes()); valid && count > 0 {
				candidates = count
			}
		}
	}
	return ResponseCodec{native: native, caller: caller, route: route, encoding: encoding, candidates: candidates}
}

func (c ResponseCodec) Native(body []byte) (*wire.Completion, error) {
	return c.decode(body, c.native.Family)
}

func (c ResponseCodec) Project(body []byte) (*wire.Completion, error) {
	return c.decode(body, c.caller.Family)
}

func (c ResponseCodec) ProjectDecoded(body []byte, native *wire.Completion) (*wire.Completion, error) {
	if c.native.Family == c.caller.Family && native != nil {
		return native, nil
	}
	return c.Project(body)
}

func (c ResponseCodec) decode(body []byte, target wire.Format) (*wire.Completion, error) {
	request := c.native
	if c.caller.Family == wire.Rerank {
		withProjection, err := request.WithField("return_documents", raw(rerankReturnDocuments(c.caller)))
		if err != nil {
			return nil, err
		}
		request = withProjection
	}
	return DecodeRequest(c.native.Family, target, body, c.route, c.encoding, request)
}

func (c ResponseCodec) Stream(reader io.Reader, maxEvent int, emit wire.Emit, observe func(oif.Event) error) (*wire.Completion, error) {
	return StreamWithEvents(c.native.Family, c.caller.Family, reader, maxEvent, c.route, c.caller.IncludeUsage, emit, observe, c.candidates)
}

func (c ResponseCodec) StreamNative(reader io.Reader, maxEvent int, emit wire.Emit, observe func(oif.Event) error) (*wire.Completion, error) {
	return StreamWithEvents(c.native.Family, c.native.Family, reader, maxEvent, c.route, true, emit, observe, c.candidates)
}
