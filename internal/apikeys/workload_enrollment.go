package apikeys

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/keyauthority"
	"github.com/tyk-swe/olp/internal/workload"
)

var workloadKeyNamespace = uuid.MustParse("f6f67f12-d053-5d5a-8073-0b0b95eb61ce")

type WorkloadSubject struct {
	IssuerID, CreatedBy, Digest string
	Mapping                     workload.Mapping
}

func EnrollWorkloadKey(ctx context.Context, tx pgx.Tx, subject WorkloadSubject) error {
	id := uuid.NewSHA1(workloadKeyNamespace, []byte(subject.Digest)).String()
	lookup := "wi_" + subject.Digest[:32]
	policy, err := json.Marshal(keyauthority.WorkloadPolicy(subject.Mapping))
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO olp.api_keys(id,lookup_id,digest,name,created_by,policy,etag,project_id,workload_issuer_id,workload_digest,workload_mapping)
 VALUES($1,$2,$3,$4,$5,$6,$1,$7,$8,$9,$10) ON CONFLICT(workload_digest) WHERE workload_digest IS NOT NULL DO NOTHING`, id, lookup, make([]byte, 32), "Workload "+subject.Digest[:12], subject.CreatedBy, policy, subject.Mapping.ProjectID, subject.IssuerID, subject.Digest, subject.Mapping.Name)
	return err
}
