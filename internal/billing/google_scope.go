package billing

import (
	"bytes"
	"encoding/json"
	"io"
	"regexp"
	"slices"

	"github.com/tyk-swe/olp/internal/access"
)

type googleScope struct {
	BillingAccountID  string   `json:"billing_account_id"`
	BigQueryProjectID string   `json:"bigquery_project_id"`
	DatasetID         string   `json:"dataset_id"`
	TableID           string   `json:"table_id"`
	ProjectIDs        []string `json:"project_ids,omitempty"`
}

var (
	googleBillingAccount = regexp.MustCompile(`^[A-Z0-9]{6}-[A-Z0-9]{6}-[A-Z0-9]{6}$`)
	googleProjectID      = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`)
	googleDatasetID      = regexp.MustCompile(`^[A-Za-z0-9_]+$`)
	googleTableID        = regexp.MustCompile(`^[\p{L}\p{M}\p{N}\p{Pc}\p{Pd}\p{Zs}]+$`)
)

func validateGoogleScope(raw json.RawMessage) (json.RawMessage, error) {
	var scope googleScope
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&scope); err != nil {
		return nil, access.Invalid("scope", "Google scope must be an object containing only supported fields")
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, access.Invalid("scope", "scope must be a single JSON document")
	}
	if !googleBillingAccount.MatchString(scope.BillingAccountID) || !googleProjectID.MatchString(scope.BigQueryProjectID) ||
		len(scope.DatasetID) > 1024 || len(scope.TableID) > 1024 || !googleDatasetID.MatchString(scope.DatasetID) || !googleTableID.MatchString(scope.TableID) {
		return nil, access.Invalid("scope", "Google billing account, BigQuery project, dataset and table must be valid identifiers")
	}
	if len(scope.ProjectIDs) > 100 {
		return nil, access.Invalid("scope.project_ids", "at most 100 project IDs")
	}
	for _, id := range scope.ProjectIDs {
		if !googleProjectID.MatchString(id) {
			return nil, access.Invalid("scope.project_ids", "project IDs must be valid Google project identifiers")
		}
	}
	return json.Marshal(googleScope{
		BillingAccountID: scope.BillingAccountID, BigQueryProjectID: scope.BigQueryProjectID,
		DatasetID: scope.DatasetID, TableID: scope.TableID, ProjectIDs: slices.Sorted(slices.Values(scope.ProjectIDs)),
	})
}
