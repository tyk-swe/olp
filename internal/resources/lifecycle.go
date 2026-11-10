package resources

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/retainedapi"
)

var ErrSubmissionConflict = errors.New("retained submission identity conflicts")

var ErrRetainedMetadata = errors.New("retained creation metadata is invalid")

type CreationObservation struct {
	Endpoint retainedapi.Endpoint
	Document oif.Document
	At       time.Time
}

type CreationResult struct {
	IDs         []string
	Observation *CreationObservation
}

type Submission struct {
	Operation string
	Key       string
	Digest    []byte
}

type lifecycleAction uint8

const (
	lifecycleRead lifecycleAction = iota
	lifecycleCreate
	lifecycleMutation
)

type Lifecycle struct {
	store      *Store
	resource   *Resource
	submission Submission
	action     lifecycleAction
	fresh      bool
}

func (s *Store) BeginCreate(ctx context.Context, candidate *Resource, submission Submission) (*Lifecycle, error) {
	resource, fresh, err := s.prepareLifecycle(ctx, candidate, submission.Operation, submission.Key, submission.Digest)
	if err != nil {
		return nil, err
	}
	return &Lifecycle{store: s, resource: resource, submission: submission, action: lifecycleCreate, fresh: fresh}, nil
}

func (s *Store) BeginMutation(ctx context.Context, resource *Resource, submission Submission) (*Lifecycle, error) {
	if err := s.prepareRetainedMutation(ctx, resource, submission.Operation, submission.Key, submission.Digest, true); err != nil {
		return nil, err
	}
	return &Lifecycle{store: s, resource: resource, submission: submission, action: lifecycleMutation}, nil
}

func (s *Store) BeginDeletion(ctx context.Context, resource *Resource) (*Lifecycle, error) {
	if resource == nil || resource.Kind != "context_cache" {
		return nil, ErrContract
	}
	identity, err := uuid.NewV7()
	if err != nil {
		return nil, err
	}
	key := identity.String()
	digest := sha256.Sum256([]byte(resource.ID + ":delete:" + key))
	lifecycle := &Lifecycle{store: s, resource: resource, action: lifecycleMutation,
		submission: Submission{Operation: "context_cache:delete", Key: key, Digest: digest[:]},
	}
	return lifecycle, s.prepareRetainedMutation(ctx, resource, "context_cache:delete", key, digest[:], false)
}

func (s *Store) OpenOwned(ctx context.Context, kind, owner, id string) (*Lifecycle, error) {
	resource, err := s.Get(ctx, kind, owner, id)
	if err != nil {
		return nil, err
	}
	return &Lifecycle{store: s, resource: resource}, nil
}

func (l *Lifecycle) Resource() *Resource { return l.resource }

func (l *Lifecycle) Existing() bool { return l.action == lifecycleCreate && !l.fresh }

func (l *Lifecycle) Dispatch(ctx context.Context) error {
	if l.action != lifecycleCreate || !l.fresh {
		return ErrSubmissionConflict
	}
	return l.store.dispatchLifecycle(ctx, l.resource.UUID)
}

func (l *Lifecycle) Close(ctx context.Context) error {
	if l == nil {
		return nil
	}
	switch l.action {
	case lifecycleCreate:
		return l.store.unknownLifecycle(ctx, l.resource.UUID)
	case lifecycleMutation:
		return l.store.unknownRetainedMutation(ctx, l.resource.UUID, l.submission.Key, l.submission.Operation)
	default:
		return nil
	}
}

func (l *Lifecycle) Reject(ctx context.Context) error {
	if l == nil || l.action == lifecycleRead {
		return nil
	}
	return l.store.rejectRetained(ctx, l.resource, l.submission.Key, l.submission.Operation, l.action == lifecycleCreate)
}

func (l *Lifecycle) AcceptMutation(ctx context.Context) error {
	if l == nil || l.action == lifecycleRead {
		return nil
	}
	if l.action != lifecycleMutation {
		return ErrSubmissionConflict
	}
	return l.store.acceptRetainedMutation(ctx, l.resource.UUID, l.submission.Key, l.submission.Operation)
}

func (l *Lifecycle) AcceptCreated(ctx context.Context, result CreationResult) (*Resource, error) {
	if l == nil || l.action != lifecycleCreate {
		return nil, ErrSubmissionConflict
	}
	resource := *l.resource
	resource.State = "active"
	var children []*Resource
	if len(result.IDs) == 0 {
		resource.UpstreamID, resource.State = "empty:"+resource.UUID.String(), StateDeleted
	} else {
		resource.UpstreamID = result.IDs[0]
		for _, id := range result.IDs[1:] {
			child := resource
			child.UUID, child.ID, child.UpstreamID = uuid.Nil, "", id
			child.SubmissionID = nil
			children = append(children, &child)
		}
	}
	if observation := result.Observation; observation != nil {
		if err := EnrichRetained(&resource, observation.Endpoint, observation.Document, observation.At); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrRetainedMetadata, err)
		}
	}
	if err := l.store.completeLifecycleChildren(ctx, &resource, children); err != nil {
		return nil, err
	}
	return &resource, nil
}

// LookupLifecycle resolves the caller's original submission before any fresh
// target selection. The stored digest describes caller input, not its binding.
func (s *Store) LookupLifecycle(ctx context.Context, owner, operation, key string, digest []byte) (*Resource, error) {
	return lookupLifecycle(ctx, s.pool, owner, operation, key, digest)
}

func lookupLifecycle(ctx context.Context, query resourceRowQuerier, owner, operation, key string, digest []byte) (*Resource, error) {
	if len(key) < 1 || len(key) > 128 || len(digest) != 32 {
		return nil, ErrSubmissionConflict
	}
	var id uuid.UUID
	var priorDigest []byte
	err := query.QueryRow(ctx, `SELECT resource_id,request_digest FROM olp.retained_submissions WHERE api_key_id=$1 AND operation=$2 AND submission_key=$3`, owner, operation, key).Scan(&id, &priorDigest)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(priorDigest, digest) {
		return nil, ErrSubmissionConflict
	}
	return scan(query.QueryRow(ctx, `SELECT `+columns+` FROM olp.provider_resources WHERE id=$1 AND api_key_id=$2`, id, owner))
}

// prepareLifecycle atomically creates the owner mapping and submission fence.
// A replay can inspect its original resource, but never acquires dispatch rights.
func (s *Store) prepareLifecycle(ctx context.Context, resource *Resource, operation, key string, digest []byte) (*Resource, bool, error) {
	if len(key) < 1 || len(key) > 128 || len(digest) != 32 {
		return nil, false, ErrSubmissionConflict
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback(ctx)
	// The same caller-supplied key may race on different gateway replicas.
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, resource.APIKeyID+":"+operation+":"+key); err != nil {
		return nil, false, err
	}
	prior, err := lookupLifecycle(ctx, tx, resource.APIKeyID, operation, key, digest)
	if err == nil {
		return prior, false, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, false, err
	}
	if resource.UUID == uuid.Nil {
		resource.UUID = uuid.Must(uuid.NewV7())
	}
	resource.UpstreamID, resource.State = "pending:"+resource.UUID.String(), "submitting"
	out, err := s.PutTx(ctx, tx, resource)
	if err != nil {
		return nil, false, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO olp.retained_submissions(resource_id,api_key_id,operation,submission_key,request_digest,state) VALUES($1,$2,$3,$4,$5,'prepared')`, out.UUID, out.APIKeyID, operation, key, digest)
	if err != nil {
		return nil, false, err
	}
	if err = s.prepareRetainedTx(ctx, tx, resource); err != nil {
		return nil, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, false, err
	}
	return out, true, nil
}

func (s *Store) dispatchLifecycle(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `UPDATE olp.retained_submissions SET state='dispatched',updated_at=now() WHERE resource_id=$1 AND state='prepared'`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrSubmissionConflict
	}
	return nil
}

func (s *Store) unknownLifecycle(ctx context.Context, id uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `WITH changed AS (UPDATE olp.retained_submissions SET state='submission_unknown',updated_at=now() WHERE resource_id=$1 AND state='dispatched' RETURNING resource_id) UPDATE olp.provider_resources SET state='submission_unknown',updated_at=now() WHERE id IN (SELECT resource_id FROM changed)`, id)
	return err
}

// completeLifecycleChildren commits multi-object create results under the same
// submission fence. The primary identity and all siblings become visible at once.
func (s *Store) completeLifecycleChildren(ctx context.Context, r *Resource, children []*Resource) error {
	if r.UpstreamID == "" || len(r.UpstreamID) > 512 || len(r.Metadata) > metadataLimit {
		return ErrContract
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `UPDATE olp.retained_submissions SET state='accepted',updated_at=now() WHERE resource_id=$1 AND state IN ('dispatched','submission_unknown')`, r.UUID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrSubmissionConflict
	}
	err = tx.QueryRow(ctx, `UPDATE olp.provider_resources SET upstream_id=$2,state=$3,metadata=$4,expires_at=$5,updated_at=clock_timestamp() WHERE id=$1 RETURNING updated_at`, r.UUID, r.UpstreamID, r.State, r.Metadata, r.ExpiresAt).Scan(&r.UpdatedAt)
	if err != nil {
		return fmt.Errorf("complete retained resource: %w", err)
	}
	for _, child := range children {
		if _, err = s.PutTx(ctx, tx, child); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) GetByUUID(ctx context.Context, id uuid.UUID, owner string) (*Resource, error) {
	r, err := scan(s.pool.QueryRow(ctx, `SELECT `+columns+` FROM olp.provider_resources WHERE id=$1 AND api_key_id=$2`, id, owner))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return r, err
}

// BindDiscovered is only for a provider-generated child observed through its
// owner-authorized parent. Serializing discovery makes repeated pages stable.
// A correlated in-flight submission keeps its original ownership and billing
// pins; discovery queues reconciliation rather than inserting another identity.
func (s *Store) BindDiscovered(ctx context.Context, child *Resource, submission ...uuid.UUID) (*Resource, error) {
	if child.ParentID == nil {
		return nil, ErrContract
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, child.APIKeyID+":"+child.ParentID.String()+":"+child.Kind+":"+child.UpstreamID); err != nil {
		return nil, err
	}
	bound, err := s.getByUpstream(ctx, tx, child.NativeIdentity(), true)
	if err == nil {
		return bound, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	if len(submission) > 0 {
		original, lookupErr := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM olp.provider_resources WHERE id=$1 AND api_key_id=$2`, submission[0], child.APIKeyID))
		if lookupErr == nil {
			if original.Kind != child.Kind || original.RouteSlug != child.RouteSlug || original.RouteRevisionID != child.RouteRevisionID || original.ProviderID != child.ProviderID || original.ProviderRevisionID != child.ProviderRevisionID || original.TargetID != child.TargetID || original.SlotID != child.SlotID || !sameOptional(original.CredentialID, child.CredentialID) || original.ParentID == nil || *original.ParentID != *child.ParentID || !sameOptional(original.ContractVersion, child.ContractVersion) {
				return nil, ErrContract
			}
			if original.UpstreamID != child.UpstreamID {
				if original.UpstreamID != "pending:"+original.UUID.String() || original.State == "not_accepted" {
					return nil, ErrContract
				}
				var state string
				if err := tx.QueryRow(ctx, `SELECT state FROM olp.retained_submissions WHERE resource_id=$1`, original.UUID).Scan(&state); err != nil {
					return nil, err
				}
				if state == "accepted" {
					current, err := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM olp.provider_resources WHERE id=$1 AND api_key_id=$2`, original.UUID, child.APIKeyID))
					if err != nil {
						return nil, err
					}
					if current.UpstreamID != child.UpstreamID {
						return nil, ErrContract
					}
					original = current
				} else if state != "dispatched" && state != "submission_unknown" {
					return nil, ErrContract
				} else if err := s.RequestRetainedReconciliationTx(ctx, tx, original.UUID, child.UpstreamID); err != nil {
					return nil, err
				}
			}
			if err := tx.Commit(ctx); err != nil {
				return nil, err
			}
			return original, nil
		}
		if !errors.Is(lookupErr, pgx.ErrNoRows) {
			return nil, lookupErr
		}
	}
	bound, err = s.PutTx(ctx, tx, child)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return bound, nil
}

// Child checks both the child's owner and its immutable parent relationship.
func (s *Store) Child(ctx context.Context, kind, owner, id string, parent *Resource) (*Resource, error) {
	r, err := s.Get(ctx, kind, owner, id)
	if err != nil {
		return nil, err
	}
	if parent == nil || r.ParentID == nil || *r.ParentID != parent.UUID {
		return nil, ErrNotFound
	}
	return r, nil
}
