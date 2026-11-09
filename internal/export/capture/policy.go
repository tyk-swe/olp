package capture

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"math/big"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/management/contract"
)

type Include = contract.CapturePolicyInclude

const (
	Input     = contract.CapturePolicyIncludeInput
	Output    = contract.CapturePolicyIncludeOutput
	ToolCalls = contract.CapturePolicyIncludeToolCalls
)

const (
	maxPolicies   = 1000
	maxRecord     = 1 << 20
	fieldMax      = 65536
	maxSelectors  = 256
	minPolicySize = 1024
)

var ratioPattern = regexp.MustCompile(`^(0(\.[0-9]{1,9})?|1(\.0{1,9})?)$`)

var inputFields = []string{"messages", "input", "contents", "system", "prompt", "preamble", "message", "chat_history", "suffix", "instructions", "systemInstruction"}

type Redactor struct {
	Name        string          `json:"name"`
	GuardrailID string          `json:"guardrail_id"`
	RevisionID  string          `json:"revision_id"`
	Revision    int64           `json:"revision"`
	Type        guardrails.Kind `json:"type"`
}

type Settings struct {
	ProjectID   *string   `json:"project_id"`
	Route       *string   `json:"route_slug"`
	SinkID      string    `json:"sink"`
	SampleRatio string    `json:"sample_ratio"`
	Include     []Include `json:"include"`
	Redact      []string  `json:"redact"`
	KeyIDs      []string  `json:"key_ids"`
	EndUsers    []string  `json:"end_user_digests"`
	MaxBytes    int       `json:"max_bytes"`
	Enabled     bool      `json:"enabled"`
}

type Policy struct {
	ID string `json:"id"`
	Settings
	RedactorRevisions []Redactor `json:"redactor_revisions"`
	ETag              string     `json:"etag"`
}

func invalid(message string) error {
	return access.Invalid("capture_policy", message)
}

func (p Settings) validate() error {
	if p.ProjectID == nil && p.Route == nil {
		return invalid("scope the policy to a project or a route")
	}
	if !ratioPattern.MatchString(p.SampleRatio) {
		return invalid("sample_ratio must be a decimal from 0 to 1 with at most nine decimal places")
	}
	if len(p.Include) < 1 || len(p.Include) > 3 {
		return invalid("include must contain input, output or tool_calls")
	}
	if !distinct(p.Include, func(i Include) (Include, bool) { return i, i.Valid() }) {
		return invalid("include must contain distinct input, output or tool_calls values")
	}
	if len(p.Redact) > guardrails.MaxEntries {
		return invalid("redact supports at most 128 local detectors")
	}
	if !distinct(p.Redact, func(name string) (string, bool) { return strings.ToLower(name), detectorName(name) }) {
		return invalid("redact must contain distinct detector names of 1 to 100 characters")
	}
	if len(p.KeyIDs) > maxSelectors || len(p.EndUsers) > maxSelectors {
		return invalid("key_ids and end_user_digests support at most 256 values")
	}
	if slices.ContainsFunc(p.KeyIDs, func(id string) bool { return !canonicalUUID(id) }) {
		return invalid("key_ids must contain canonical UUIDs")
	}
	if slices.ContainsFunc(p.EndUsers, func(digest string) bool { return len(digest) != 64 || strings.Trim(digest, "0123456789abcdef") != "" }) {
		return invalid("end_user_digests must contain project-scoped SHA-256 digests")
	}
	if p.MaxBytes < minPolicySize || p.MaxBytes > maxRecord {
		return invalid("max_bytes must be from 1024 to 1048576")
	}
	return nil
}

func distinct[T any, K comparable](values []T, key func(T) (K, bool)) bool {
	seen := make(map[K]bool, len(values))
	for _, value := range values {
		k, ok := key(value)
		if !ok || seen[k] {
			return false
		}
		seen[k] = true
	}
	return true
}

func detectorName(name string) bool {
	return name != "" && strings.TrimSpace(name) == name && utf8.ValidString(name) &&
		utf8.RuneCountInString(name) <= 100 && !strings.ContainsFunc(name, unicode.IsControl)
}

func canonicalUUID(id string) bool {
	parsed, err := uuid.Parse(id)
	return err == nil && parsed.String() == id
}

func (p Settings) matches(project, route, key, endUser string) bool {
	return (p.ProjectID == nil || *p.ProjectID == project) && (p.Route == nil || *p.Route == route) &&
		(len(p.KeyIDs) == 0 || slices.Contains(p.KeyIDs, key)) &&
		(len(p.EndUsers) == 0 || slices.Contains(p.EndUsers, endUser))
}

func sampled(ratio *big.Rat, policyID, requestID string) bool {
	digest := sha256.Sum256([]byte(policyID + "|" + requestID))
	value := new(big.Int).SetUint64(binary.BigEndian.Uint64(digest[:8]))
	left := new(big.Int).Mul(value, ratio.Denom())
	right := new(big.Int).Lsh(new(big.Int).Set(ratio.Num()), 64)
	return left.Cmp(right) < 0
}

func inputOf(document map[string]json.RawMessage) map[string]json.RawMessage {
	result := map[string]json.RawMessage{}
	for _, field := range inputFields {
		if value := document[field]; len(value) != 0 {
			result[field] = slices.Clone(value)
		}
	}
	return result
}

func objectKey(requestID string) string {
	return "captures/" + strings.ReplaceAll(requestID, "-", "") + ".jsonl"
}
