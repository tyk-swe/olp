package usage

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/tyk-swe/olp/internal/access"
)

const csvMaxBytes = 32 << 20

type csvBuffer struct {
	bytes.Buffer
}

func (b *csvBuffer) Write(data []byte) (int, error) {
	if len(data) > csvMaxBytes-b.Len() {
		return 0, access.Fail(422, "export_too_large", "Narrow the filters to produce a CSV export of at most 32 MiB.")
	}
	return b.Buffer.Write(data)
}

func csvCell(value string) string {
	start := strings.TrimLeftFunc(value, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) || r == '\uFEFF' })
	if start != "" && strings.ContainsRune("=+-@", rune(start[0])) ||
		strings.HasPrefix(value, "\t") || strings.HasPrefix(value, "\r") || strings.HasPrefix(value, "\n") {
		return "'" + value
	}
	return value
}

func csvOptional[T string | bool | int32 | int64](value *T) string {
	if value == nil {
		return ""
	}
	return fmt.Sprint(*value)
}

func RequestCSV(items []RequestSummary) ([]byte, error) {
	if len(items) > 10000 {
		return nil, access.Fail(422, "export_too_large", "Narrow the request filters to at most 10000 requests.")
	}
	var buffer csvBuffer
	writer := csv.NewWriter(&buffer)
	writer.UseCRLF = true
	if err := writer.Write([]string{
		"request_id", "runtime_generation_id", "api_key_id", "origin", "parent_request_id", "route", "operation", "surface",
		"started_at", "completed_at", "status_code", "error_class", "total_latency_ms", "first_byte_ms", "attempt_count",
		"input_tokens", "output_tokens", "cached_input_tokens", "cache_write_input_tokens", "cache_write_5m_input_tokens", "cache_write_1h_input_tokens",
		"estimated_cost", "currency", "unpriced", "usage_complete", "end_user_digest", "session_id", "payload_captured",
	}); err != nil {
		return nil, err
	}
	for _, item := range items {
		completed := ""
		if item.CompletedAt != nil {
			completed = item.CompletedAt.UTC().Format(time.RFC3339Nano)
		}
		row := []string{
			item.ID, item.RuntimeGenerationID, csvOptional(item.APIKeyID), item.Origin, csvOptional(item.ParentRequestID), item.Route, item.Operation, item.Surface,
			item.StartedAt.UTC().Format(time.RFC3339Nano), completed, csvOptional(item.StatusCode), csvOptional(item.ErrorClass),
			csvOptional(item.TotalLatencyMS), csvOptional(item.FirstByteMS), strconv.FormatInt(int64(item.AttemptCount), 10),
			csvOptional(item.InputTokens), csvOptional(item.OutputTokens), csvOptional(item.CachedInputTokens), csvOptional(item.CacheWriteInputTokens),
			csvOptional(item.CacheWrite5MInputTokens), csvOptional(item.CacheWrite1HInputTokens), csvOptional(item.EstimatedCost), csvOptional(item.Currency),
			csvOptional(item.Unpriced), csvOptional(item.UsageComplete), item.EndUserDigest, item.Attribution["session"], strconv.FormatBool(item.PayloadCaptured),
		}
		for i, value := range row {
			row[i] = csvCell(value)
		}
		if err := writer.Write(row); err != nil {
			return nil, err
		}
	}
	writer.Flush()
	return buffer.Bytes(), writer.Error()
}

func UsageCSV(filters Filters, dimension string, summary Summary, breakdown Breakdown) ([]byte, error) {
	if len(breakdown.Items) > 10000 {
		return nil, access.Fail(422, "export_too_large", "Narrow the usage filters to at most 10000 groups.")
	}
	var buffer csvBuffer
	writer := csv.NewWriter(&buffer)
	writer.UseCRLF = true
	if err := writer.Write([]string{
		"row_type", "dimension", "dimension_value", "start", "end", "request_count", "input_tokens", "output_tokens", "cached_input_tokens",
		"cache_write_input_tokens", "cache_write_5m_input_tokens", "cache_write_1h_input_tokens", "media_units", "estimated_cost", "currency",
		"unpriced_count", "incomplete_count", "range_complete", "approximate", "excluded_partial_aggregate_boundaries", "complete",
		"request_metadata_gap_events", "uncertain_request_metadata_gap_count", "estimated_input_tokens", "reported_input_tokens", "estimated_attempt_count",
	}); err != nil {
		return nil, err
	}
	write := func(kind, value string, totals Totals, coverage Coverage) error {
		if !totals.valid() {
			return errInvalidUsageCount
		}
		row := []string{
			kind, dimension, value, filters.Start.UTC().Format(time.RFC3339Nano), filters.End.UTC().Format(time.RFC3339Nano),
			strconv.FormatInt(totals.RequestCount, 10), totals.InputTokens, totals.OutputTokens, totals.CachedInputTokens, totals.CacheWriteInputTokens,
			totals.CacheWrite5MInputTokens, totals.CacheWrite1HInputTokens, totals.MediaUnits, csvOptional(totals.EstimatedCost), csvOptional(totals.Currency),
			strconv.FormatInt(totals.UnpricedCount, 10), strconv.FormatInt(totals.IncompleteCount, 10), strconv.FormatBool(coverage.RangeComplete),
			strconv.FormatBool(coverage.Approximate), strconv.Itoa(coverage.ExcludedBoundaries), strconv.FormatBool(summary.Complete),
			strconv.FormatInt(summary.GapEvents, 10), strconv.FormatInt(summary.UncertainGapCount, 10), totals.EstimatedInputTokens, totals.ReportedInputTokens,
			strconv.FormatInt(totals.EstimatedAttemptCount, 10),
		}
		for i, value := range row {
			row[i] = csvCell(value)
		}
		return writer.Write(row)
	}
	if err := write("summary", "", summary.Totals, summary.Coverage); err != nil {
		return nil, err
	}
	for _, item := range breakdown.Items {
		if err := write("breakdown", item.Dimension, item.Totals, breakdown.Coverage); err != nil {
			return nil, err
		}
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}
