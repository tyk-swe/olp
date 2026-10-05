package media

import (
	"fmt"
	"maps"
	"slices"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/vendors"
)

// A codec translates the OpenAI media operations of one vendor wire, which
// vendor contracts name, to the vendor's API and its results back. Codecs
// serve providers without a profile on transformed routes; a vendor that
// names no wire for an operation speaks OpenAI's.
type codec struct {
	// encode is the upstream call for each operation the wire serves. The
	// call decodes the vendor's result itself where it differs from
	// OpenAI's.
	encode map[string]func(r *Request, model string) (*UpstreamCall, *Error)
	// encodeFor encodes an operation whose call depends on the connector,
	// such as one addressed at another of its cloud's services.
	encodeFor map[string]func(r *Request, cfg connectors.Config, model string) (*UpstreamCall, *Error)
}

var codecs = map[string]codec{}

// registerCodec adds a vendor wire's codec; each codec file registers its own.
func registerCodec(wire string, c codec) {
	if _, duplicate := codecs[wire]; duplicate || len(c.encode)+len(c.encodeFor) == 0 {
		panic(fmt.Sprintf("media codec %q is registered twice or serves nothing", wire))
	}
	codecs[wire] = c
}

// Wires are the registered vendor wires and the operations each serves.
func Wires() map[string][]string {
	out := map[string][]string{}
	for wire, c := range codecs {
		out[wire] = slices.Sorted(maps.Keys(c.encode))
		for operation := range c.encodeFor {
			out[wire] = append(out[wire], operation)
		}
		slices.Sort(out[wire])
	}
	return out
}

// encodeVendor encodes a request in the vendor's own wire, reporting whether
// the vendor has one for the operation.
func encodeVendor(r *Request, cfg connectors.Config, model string) (*UpstreamCall, bool, *Error) {
	wire := vendors.MediaWire(cfg.VendorID, r.Op)
	if wire == "" {
		return nil, false, nil
	}
	if encode := codecs[wire].encodeFor[r.Op]; encode != nil {
		call, failure := encode(r, cfg, model)
		return call, true, failure
	}
	encode := codecs[wire].encode[r.Op]
	if encode == nil {
		return nil, true, invalidMedia("The vendor's media codec does not serve this operation.")
	}
	call, failure := encode(r, model)
	return call, true, failure
}
