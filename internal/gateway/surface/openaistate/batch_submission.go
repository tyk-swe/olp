package openaistate

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/domain/vendors"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/gateway/managedbatch"
)

var errUnsupportedBatchEndpoint = fault.InvalidRequest("unsupported_parameter", "The batch endpoint must be a supported operation.", new("endpoint"))

func batchLineOperation(source oif.Document) (string, wire.LineOperation, *fault.Error) {
	value, present := source.Root().Lookup("endpoint")
	endpoint, valid := value.Text()
	if !present || !valid {
		return "", "", errUnsupportedBatchEndpoint
	}
	operation, ok := wire.BatchLineOperation(endpoint)
	if !ok {
		return "", "", errUnsupportedBatchEndpoint
	}
	return endpoint, operation, nil
}

func batchCompletionWindow(source oif.Document, vendor string) (managedbatch.Window, *fault.Error) {
	value, present := source.Root().Lookup("completion_window")
	if !present {
		return managedbatch.Window{Label: "24h", Length: kernel.DefaultBatchCompletionWindow}, nil
	}
	raw, valid := value.Text()
	if !valid {
		return managedbatch.Window{}, fault.InvalidRequest("invalid_value", "completion_window must be a valid duration.", new("completion_window"))
	}
	hours, ok := windowHours(raw)
	if !ok {
		return managedbatch.Window{}, fault.InvalidRequest("invalid_value", "completion_window must use whole hours or days.", new("completion_window"))
	}
	served, _ := vendors.BatchWindowFor(vendor)
	if hours < served.MinHours || hours > served.MaxHours {
		return managedbatch.Window{}, fault.InvalidRequest("invalid_value", "completion_window is outside the provider's supported range.", new("completion_window"))
	}
	return managedbatch.Window{Label: raw, Length: time.Duration(hours) * time.Hour}, nil
}

func windowHours(raw string) (int, bool) {
	if len(raw) < 2 {
		return 0, false
	}
	n, err := strconv.Atoi(raw[:len(raw)-1])
	if err != nil || n <= 0 {
		return 0, false
	}
	switch raw[len(raw)-1] {
	case 'h':
		return n, n <= 168
	case 'd':
		return n * 24, n <= 7
	default:
		return 0, false
	}
}

func batchEndpointRefusal(p *kernel.Pin, endpoint, pointer string) *fault.Error {
	served := vendors.BatchEndpointsFor(p.Provider.VendorID)
	if len(served) == 0 || slices.Contains(served, endpoint) {
		return nil
	}
	return fault.InvalidRequest("unsupported_parameter", "This provider's batches support only "+strings.Join(served, ", ")+".", new(pointer))
}

func batchExpectedResults(body []byte) *int64 {
	var response struct {
		RequestCounts *struct {
			Total *int64 `json:"total"`
		} `json:"request_counts"`
	}
	if json.Unmarshal(body, &response) != nil || response.RequestCounts == nil ||
		response.RequestCounts.Total == nil || *response.RequestCounts.Total < 0 {
		return nil
	}
	count := *response.RequestCounts.Total
	return &count
}
