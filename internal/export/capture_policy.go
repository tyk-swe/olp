package export

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math/big"
	"regexp"
	"slices"
	"strings"

	"github.com/google/uuid"
)

var captureRatioPattern = regexp.MustCompile(`^(0(\.[0-9]{1,9})?|1(\.0{1,9})?)$`)

type CapturePolicy struct {
	ID          string   `json:"id"`
	ProjectID   *string  `json:"project_id"`
	Route       *string  `json:"route_slug"`
	SinkID      string   `json:"sink"`
	SampleRatio string   `json:"sample_ratio"`
	Include     []string `json:"include"`
	Redact      []string `json:"redact"`
	KeyIDs      []string `json:"key_ids"`
	EndUsers    []string `json:"end_user_digests"`
	MaxBytes    int      `json:"max_bytes"`
	Enabled     bool     `json:"enabled"`
	ETag        string   `json:"etag"`
}

func (p CapturePolicy) Validate() error {
	if !captureRatioPattern.MatchString(p.SampleRatio) {
		return errors.New("sample_ratio must be a decimal from 0 to 1 with at most nine decimal places")
	}
	if len(p.Include) < 1 || len(p.Include) > 3 {
		return errors.New("include must contain input, output or tool_calls")
	}
	seen := map[string]bool{}
	for _, field := range p.Include {
		if !slices.Contains([]string{"input", "output", "tool_calls"}, field) || seen[field] {
			return errors.New("include must contain distinct input, output or tool_calls values")
		}
		seen[field] = true
	}
	if len(p.Redact) != 0 {
		return errors.New("capture redaction requires M7 detectors; unredacted capture is restricted to installation sinks")
	}
	if len(p.KeyIDs) > 256 || len(p.EndUsers) > 256 {
		return errors.New("key_ids and end_user_digests support at most 256 values")
	}
	for _, id := range p.KeyIDs {
		parsed, err := uuid.Parse(id)
		if err != nil || parsed.String() != id {
			return errors.New("key_ids must contain canonical UUIDs")
		}
	}
	for _, digest := range p.EndUsers {
		if len(digest) != 64 || strings.Trim(digest, "0123456789abcdef") != "" {
			return errors.New("end_user_digests must contain project-scoped SHA-256 digests")
		}
	}
	if p.MaxBytes < 1024 || p.MaxBytes > 1048576 {
		return errors.New("max_bytes must be from 1024 to 1048576")
	}
	return nil
}

func (p CapturePolicy) Sample(requestID string) bool {
	if !p.Enabled || !captureRatioPattern.MatchString(p.SampleRatio) {
		return false
	}
	ratio, ok := new(big.Rat).SetString(p.SampleRatio)
	if !ok {
		return false
	}
	digest := sha256.Sum256([]byte(p.ID + "|" + requestID))
	value := new(big.Int).SetUint64(binary.BigEndian.Uint64(digest[:8]))
	left := new(big.Int).Mul(value, ratio.Denom())
	right := new(big.Int).Lsh(new(big.Int).Set(ratio.Num()), 64)
	return left.Cmp(right) < 0
}

func (p CapturePolicy) Includes(field string) bool { return slices.Contains(p.Include, field) }

func (p CapturePolicy) Matches(project, route, key, endUser string) bool {
	return (p.ProjectID == nil || *p.ProjectID == project) && (p.Route == nil || *p.Route == route) &&
		(len(p.KeyIDs) == 0 || slices.Contains(p.KeyIDs, key)) &&
		(len(p.EndUsers) == 0 || slices.Contains(p.EndUsers, endUser))
}

var captureInputFields = []string{"messages", "input", "contents", "system", "prompt", "preamble", "message", "chat_history", "suffix"}

func CaptureInput(document map[string]json.RawMessage) map[string]json.RawMessage {
	result := map[string]json.RawMessage{}
	for _, field := range captureInputFields {
		if value := document[field]; len(value) != 0 {
			result[field] = slices.Clone(value)
		}
	}
	return result
}

func CaptureObjectName(requestID string) string {
	return "captures/" + strings.ReplaceAll(requestID, "-", "") + ".jsonl"
}
