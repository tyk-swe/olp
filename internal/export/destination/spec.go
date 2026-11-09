package destination

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/management/contract"
)

type Type = contract.CreateExportSinkRequestType
type Format = contract.CreateExportSinkRequestFormat
type Stream = contract.CreateExportSinkRequestStreams

const (
	MeteringStream = contract.CreateExportSinkRequestStreamsMetering
	FOCUSStream    = contract.CreateExportSinkRequestStreamsFocus
)

type Spec interface {
	Type() Type
	Format() Format
	transport(credential []byte, strict bool) (transport, error)
}

type kind struct {
	format Format
	only   Stream
	parse  func(provider Type, settings json.RawMessage) (Spec, error)
}

var kinds = map[Type][]kind{
	contract.CreateExportSinkRequestTypeHttps: {
		{format: contract.CreateExportSinkRequestFormatJson, parse: parseJSONWebhook},
		{format: contract.CreateExportSinkRequestFormatCloudevents, only: MeteringStream, parse: parseCloudEvents},
	},
	contract.CreateExportSinkRequestTypeOtlpLogs: {
		{format: contract.CreateExportSinkRequestFormatOtlp, parse: parseOTLPLogs},
	},
	contract.CreateExportSinkRequestTypeS3:        objectKinds,
	contract.CreateExportSinkRequestTypeGcs:       objectKinds,
	contract.CreateExportSinkRequestTypeAzureBlob: objectKinds,
	contract.CreateExportSinkRequestTypeStripeMeter: {
		{format: contract.CreateExportSinkRequestFormatStripe, only: MeteringStream, parse: parseStripeMeter},
	},
}

var objectKinds = []kind{
	{format: contract.CreateExportSinkRequestFormatJsonl, parse: parseObjects},
	{format: contract.CreateExportSinkRequestFormatCsvGzip, only: FOCUSStream, parse: parseFOCUS},
}

func lookup(sinkType Type, format Format) (kind, error) {
	formats, ok := kinds[sinkType]
	if !ok {
		return kind{}, access.Invalid("type", "type must be https, otlp_logs, s3, gcs, azure_blob or stripe_meter")
	}
	i := slices.IndexFunc(formats, func(k kind) bool { return k.format == format })
	if i < 0 {
		names := make([]string, len(formats))
		for j, k := range formats {
			names[j] = string(k.format)
		}
		return kind{}, access.Invalid("format", fmt.Sprintf("%s sinks support format %s", sinkType, strings.Join(names, " or ")))
	}
	return formats[i], nil
}

func ParseSpec(sinkType Type, format Format, settings json.RawMessage) (Spec, error) {
	k, err := lookup(sinkType, format)
	if err != nil {
		return nil, err
	}
	return k.parse(sinkType, settings)
}

func checkStreams(k kind, streams []Stream) error {
	if len(streams) < 1 || len(streams) > 6 {
		return access.Invalid("streams", "streams must contain from 1 to 6 entries")
	}
	seen := map[Stream]bool{}
	for _, stream := range streams {
		if !stream.Valid() || seen[stream] {
			return access.Invalid("streams", "streams must contain distinct supported entries")
		}
		seen[stream] = true
	}
	switch {
	case k.only == MeteringStream && (len(streams) != 1 || streams[0] != MeteringStream):
		return access.Invalid("streams", fmt.Sprintf("%s sinks carry only the metering stream", k.format))
	case k.only == FOCUSStream && (len(streams) != 1 || streams[0] != FOCUSStream):
		return access.Invalid("streams", "csv_gzip sinks carry only the focus stream")
	case k.only != FOCUSStream && seen[FOCUSStream]:
		return access.Invalid("streams", "the focus stream requires a csv_gzip object sink")
	}
	return nil
}

func decodeSettings(format Format, raw json.RawMessage, into any, accepted ...string) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var fields map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&fields); err != nil || fields == nil {
		return access.Invalid("settings", "settings must be a JSON object")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return access.Invalid("settings", "settings must be a single JSON object")
	}
	for _, field := range slices.Sorted(maps.Keys(fields)) {
		if string(fields[field]) != "null" && !slices.Contains(accepted, field) {
			return access.Invalid("settings", fmt.Sprintf("%s sinks do not accept settings.%s", format, field))
		}
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return access.Invalid("settings", "settings must be a JSON object")
	}
	return nil
}
