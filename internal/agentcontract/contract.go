// Package agentcontract owns project-scoped agent publications and their authority
// references. It intentionally has no consumer transport or execution loop.
package agentcontract

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/google/uuid"
	grantconfig "github.com/tyk-swe/olp/internal/grants/config"
	"github.com/tyk-swe/olp/internal/prompts"
)

type Tool struct {
	Classification string `json:"classification,omitempty"`
	ServerID       string `json:"server_id"`
	RevisionID     string `json:"revision_id"`
	CatalogDigest  string `json:"catalog_digest"`
	Name           string `json:"name"`
	Alias          string `json:"alias"`
}
type Toolset struct {
	Tools []Tool `json:"tools"`
}
type Definition struct {
	Protocol               string             `json:"protocol"`
	Endpoint               string             `json:"endpoint"`
	CardDigest             string             `json:"card_digest"`
	OAuth                  *grantconfig.OAuth `json:"oauth,omitempty"`
	CredentialID           *string            `json:"credential_id,omitempty"`
	ToolsetRevisions       []string           `json:"toolset_revisions"`
	RouteRevisions         []string           `json:"route_revisions"`
	PricingRevisionID      string             `json:"pricing_revision_id"`
	MaxCost                string             `json:"max_cost"`
	MessagePrice           string             `json:"message_price,omitempty"`
	RemoteWorkBoundSeconds *int               `json:"remote_work_bound_seconds,omitempty"`
	MaxTaskContinuations   *int               `json:"max_task_continuations,omitempty"`
	TaskDeadlineSeconds    *int               `json:"task_deadline_seconds,omitempty"`
}
type Draft struct {
	ProjectID string              `json:"project_id,omitempty"`
	Name      string              `json:"name"`
	Toolset   *Toolset            `json:"toolset,omitempty"`
	Agent     *Definition         `json:"agent,omitempty"`
	Prompt    *prompts.Definition `json:"prompt,omitempty"`
}
type Publication struct {
	ID         string  `json:"id"`
	Kind       string  `json:"kind"`
	ProjectID  string  `json:"project_id"`
	Name       string  `json:"name"`
	ETag       string  `json:"etag"`
	Draft      Draft   `json:"draft"`
	RevisionID *string `json:"revision_id"`
	Retired    bool    `json:"retired"`
}

var hexDigest = regexp.MustCompile(`^[a-f0-9]{64}$`)

func validID(raw string) bool {
	id, err := uuid.Parse(raw)
	return err == nil && id != uuid.Nil && id.String() == raw
}

func ToolAlias(server, name string) string {
	digest := sha256.Sum256([]byte(name))
	return "t_" + strings.ReplaceAll(server, "-", "") + "_" + hex.EncodeToString(digest[:16])
}

func (d Draft) Validate(kind string) error {
	if !validID(d.ProjectID) {
		return invalid("project_id", "Choose an owning project.")
	}
	if err := validName(d.Name); err != nil {
		return err
	}
	count := 0
	for _, ok := range []bool{d.Toolset != nil, d.Agent != nil, d.Prompt != nil} {
		if ok {
			count++
		}
	}
	if count != 1 {
		return invalid("draft", "Supply exactly one resource definition.")
	}
	switch kind {
	case "toolset":
		if d.Toolset == nil || len(d.Toolset.Tools) > 128 {
			return invalid("toolset", "Supply at most 128 pinned tools.")
		}
		seen := map[string]bool{}
		for _, t := range d.Toolset.Tools {
			if t.Classification != "" && t.Classification != "read_only" && t.Classification != "approval_required" {
				return invalid("toolset", "Use a supported operator classification for each tool.")
			}
			if !validID(t.ServerID) || !validID(t.RevisionID) || !hexDigest.MatchString(t.CatalogDigest) || len(t.Name) == 0 || len(t.Name) > 256 || t.Alias != ToolAlias(t.ServerID, t.Name) || seen[t.Alias] {
				return invalid("toolset", "Use unique aliases and exact certified server revisions.")
			}
			seen[t.Alias] = true
		}
	case "agent":
		if d.Agent == nil {
			return invalid("agent", "Supply an agent definition.")
		}
		a := d.Agent
		if !slices.Contains([]string{"a2a-0.3.0", "a2a-1.0.0"}, a.Protocol) || !hexDigest.MatchString(a.CardDigest) || !validID(a.PricingRevisionID) || !validCost(a.MaxCost) {
			return invalid("agent", "Supply pinned protocol, card, pricing and a finite positive cost bound.")
		}
		if a.MessagePrice != "" && (!validAmount(a.MessagePrice) || !priceWithinBound(a.MessagePrice, a.MaxCost)) {
			return invalid("message_price", "Supply a nonnegative price within the cost bound.")
		}
		if a.RemoteWorkBoundSeconds != nil && (*a.RemoteWorkBoundSeconds < 1 || *a.RemoteWorkBoundSeconds > 3600) {
			return invalid("remote_work_bound_seconds", "Use a remote-work duration between one second and one hour.")
		}
		if a.MaxTaskContinuations != nil && (*a.MaxTaskContinuations < 1 || *a.MaxTaskContinuations > 128) {
			return invalid("max_task_continuations", "Use between 1 and 128 task continuations.")
		}
		if a.TaskDeadlineSeconds != nil && (*a.TaskDeadlineSeconds < 1 || *a.TaskDeadlineSeconds > 3600) {
			return invalid("task_deadline_seconds", "Use a task deadline between one second and one hour.")
		}
		if a.CredentialID != nil && !validID(*a.CredentialID) {
			return invalid("credential_id", "Use an agent credential reference.")
		}
		if a.OAuth != nil && (!a.OAuth.Valid() || (a.OAuth.EntraOBO == nil && a.CredentialID != nil)) {
			return invalid("oauth", "Use browser or device OAuth without a static credential, or supply an Entra client secret.")
		}
		if a.OAuth != nil && a.OAuth.EntraOBO != nil {
			issuer, err := url.Parse(a.OAuth.EntraOBO.Issuer)
			if err != nil || issuer.Host == "" || (issuer.Scheme != "https" && issuer.Scheme != "http") || issuer.User != nil || issuer.RawQuery != "" || issuer.Fragment != "" {
				return invalid("oauth", "Supply the exact registered workload issuer URL.")
			}
		}
		for _, ids := range [][]string{a.ToolsetRevisions, a.RouteRevisions} {
			if len(ids) > 100 {
				return invalid("agent", "Use at most 100 revision references.")
			}
			seen := map[string]bool{}
			for _, id := range ids {
				if !validID(id) || seen[id] {
					return invalid("agent", "Use unique immutable revision UUIDs.")
				}
				seen[id] = true
			}
		}
	case "prompt":
		if d.Prompt == nil || d.Prompt.Validate() != nil {
			return invalid("prompt", "Use bounded templates with declared simple string variables.")
		}
	default:
		return invalid("kind", "Unknown publication kind.")
	}
	return nil
}

func DecodeDraft(data []byte) (Draft, error) {
	var d Draft
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if dec.Decode(&d) != nil || dec.Decode(new(any)) != io.EOF {
		return d, invalid("draft", "Use a known publication definition.")
	}
	return d, nil
}

func DraftDigest(d Draft) string {
	data, _ := json.Marshal(d)
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func invalid(field, detail string) error { return fmt.Errorf("%s: %s", field, detail) }
func validName(v string) error {
	if len(strings.TrimSpace(v)) == 0 || len(v) > 100 || strings.ContainsAny(v, "\x00\r\n") {
		return invalid("name", "Use 1–100 characters.")
	}
	return nil
}
func validCost(v string) bool {
	return validAmount(v) && strings.Trim(v, "0.") != ""
}
func validAmount(v string) bool {
	return regexp.MustCompile(`^[0-9]{1,12}(\.[0-9]{1,12})?$`).MatchString(v)
}
func priceWithinBound(price, bound string) bool {
	p, priceOK := new(big.Rat).SetString(price)
	b, boundOK := new(big.Rat).SetString(bound)
	return priceOK && boundOK && p.Cmp(b) <= 0
}

type Revision struct {
	ID            string   `json:"id"`
	PublicationID string   `json:"publication_id"`
	ProjectID     string   `json:"project_id"`
	Kind          string   `json:"kind"`
	Digest        string   `json:"digest"`
	Definition    Draft    `json:"definition"`
	Current       bool     `json:"current"`
	Labels        []string `json:"labels"`
}
