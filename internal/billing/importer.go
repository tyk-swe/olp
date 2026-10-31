package billing

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/internal/usage"
)

// Importer polls configured billing imports and stores the invoiced costs
// each vendor returns. It needs only PostgreSQL and the providers' network
// paths, so it runs on any process that can reach both, independent of the
// coordination store.
type Importer struct {
	Pool         *pgxpool.Pool
	Keys         *secrets.KeyRing
	Installation string
	Egress       *egress.Policy
	Connections  *egress.ConnectionClientCache
	Log          *slog.Logger
	PassBudget   time.Duration

	mu       sync.Mutex
	outcomes map[[2]string]int64
}

const (
	// ImportInterval is how often the importer scans for due rows.
	ImportInterval = time.Minute
	// ImportPassTimeout bounds one claiming pass, not one vendor request.
	ImportPassTimeout = 2 * time.Minute

	claimBatch   = 10
	claimLease   = 10 * time.Minute
	successDelay = time.Hour
	baseBackoff  = 5 * time.Minute
	maxBackoff   = time.Hour

	maxPages      = 20
	maxBodyBytes  = 4 << 20
	requestTimout = 30 * time.Second

	purgeBatch = 5000
)

// fetchError is a vendor fetch that failed for a reason a retry can read as a
// single content-free code; the underlying error never reaches the database.
type fetchError struct {
	code string
	err  error
}

func (e *fetchError) Error() string { return e.err.Error() }
func (e *fetchError) Unwrap() error { return e.err }

func failFetch(code string, err error) *fetchError { return &fetchError{code: code, err: err} }

// fetchCode maps any import failure onto the stored status codes. Vendor 401s
// and 403s mean the credential is the problem.
func fetchCode(err error) string {
	var fe *fetchError
	if errors.As(err, &fe) {
		return fe.code
	}
	return "network"
}

// Metrics returns the process-local per-provider import outcome counts.
func (i *Importer) Metrics() map[[2]string]int64 {
	i.mu.Lock()
	defer i.mu.Unlock()
	out := make(map[[2]string]int64, len(i.outcomes))
	for key, count := range i.outcomes {
		out[key] = count
	}
	return out
}

func (i *Importer) outcome(provider, outcome string) {
	i.mu.Lock()
	if i.outcomes == nil {
		i.outcomes = map[[2]string]int64{}
	}
	i.outcomes[[2]string{provider, outcome}]++
	i.mu.Unlock()
}

// Run executes the claim loop until ctx is cancelled, checkpointing its
// liveness after every pass the way the export worker does.
func (i *Importer) Run(ctx context.Context) {
	for {
		budget := i.PassBudget
		if budget <= 0 {
			budget = ImportPassTimeout
		}
		passCtx, cancel := context.WithTimeout(ctx, budget)
		progress, err := i.pass(passCtx)
		cancel()
		outcome := usage.OutcomeSuccess
		if err != nil {
			outcome = usage.OutcomeFailure
			i.log().Warn("billing import pass failed", "code", "database")
		}
		checkpoint, done := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		if err := usage.CheckpointTask(checkpoint, i.Pool, usage.TaskBillingImport, outcome, progress); err != nil {
			i.log().Warn("billing import checkpoint failed", "code", "database")
		}
		done()
		if ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(ImportInterval):
		}
	}
}

// Pass runs one claiming pass, for tests and the loop alike.
func (i *Importer) Pass(ctx context.Context) error {
	_, err := i.pass(ctx)
	return err
}

func (i *Importer) log() *slog.Logger {
	if i.Log != nil {
		return i.Log
	}
	return slog.Default()
}

const claimSQL = `UPDATE olp.provider_billing_imports
	SET next_import_at = now() + interval '10 minutes'
	WHERE provider_id IN (
		SELECT provider_id FROM olp.provider_billing_imports
		WHERE enabled AND next_import_at <= now()
		ORDER BY next_import_at
		LIMIT 10
		FOR UPDATE SKIP LOCKED
	)
	RETURNING ` + importColumns

// pass claims the due rows, runs each import, then purges expired invoice
// rows. Claiming and each import commit separately so one slow vendor cannot
// hold the batch.
func (i *Importer) pass(ctx context.Context) (bool, error) {
	var due []importRow
	rows, err := i.Pool.Query(ctx, claimSQL)
	if err != nil {
		return false, err
	}
	for rows.Next() {
		row, err := scanImport(rows)
		if err != nil {
			rows.Close()
			return false, err
		}
		due = append(due, *row)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}
	for _, row := range due {
		if err := i.importOne(ctx, &row); err != nil {
			code := fetchCode(err)
			if err := i.recordFailure(ctx, &row, code); err != nil {
				return true, err
			}
			i.outcome(row.ProviderID, code)
			i.log().Warn("billing import failed", "provider", row.ProviderID, "code", code)
		} else {
			i.outcome(row.ProviderID, "success")
		}
	}
	return len(due) > 0, i.purge(ctx)
}

// window is the fetch range for one import: the first successful import looks
// back a month; later ones refresh the last week, so vendor restatements
// reach the store.
func window(through *string, today time.Time) (start, end time.Time) {
	end = today
	if through == nil {
		return end.AddDate(0, 0, -30), end
	}
	return end.AddDate(0, 0, -7), end
}

// importOne fetches one provider's window and commits its days.
func (i *Importer) importOne(ctx context.Context, row *importRow) error {
	cfg, err := loadProviderConfig(ctx, i.Pool, row.ProviderID)
	if err != nil {
		return failFetch("endpoint", err)
	}
	if row.CredentialID == nil {
		return failFetch("credential", errors.New("no billing credential is stored"))
	}
	stored, err := i.Keys.Read(ctx, i.Pool, i.Installation, *row.CredentialID, secrets.BillingCredential)
	if err != nil {
		return failFetch("credential", errors.New("billing credential could not be read"))
	}
	defer clear(stored)
	key, err := decodeCredential(row.Source, stored)
	if err != nil {
		return failFetch("credential", err)
	}
	defer clear(key)
	client, err := i.client(ctx, row.ProviderID, cfg, key)
	if err != nil {
		return failFetch("endpoint", err)
	}
	var scope Scope
	if err := json.Unmarshal(row.Scope, &scope); err != nil {
		return failFetch("response", err)
	}
	today := time.Now().UTC().Truncate(24 * time.Hour)
	start, end := window(row.ImportedThrough, today)
	var days map[string]map[lineKey]lineSum
	switch row.Source {
	case SourceOpenAI:
		days, err = fetchOpenAI(ctx, client, cfg.Endpoint, key, scope, start, end)
	case SourceAnthropic:
		days, err = fetchAnthropic(ctx, client, cfg.Endpoint, key, scope, start, end)
	default:
		err = failFetch("response", errors.New("unsupported billing source"))
	}
	if err != nil {
		return err
	}
	return i.storeDays(ctx, row, "", nil, days)
}

// client builds the connection-scoped HTTP client the import's provider
// configuration calls for, honoring its network options and credential.
func (i *Importer) client(ctx context.Context, providerID string, cfg *providerConfig, credential []byte) (*http.Client, error) {
	if cfg.Endpoint == nil || *cfg.Endpoint == "" {
		return nil, errors.New("the provider has no endpoint")
	}
	if _, err := i.Egress.ValidateEndpoint(*cfg.Endpoint); err != nil {
		return nil, err
	}
	var secret []byte
	if options := cfg.Options.Network; options != nil && options.CredentialID != "" {
		data, err := i.Keys.Read(ctx, i.Pool, i.Installation, options.CredentialID, secrets.ProviderCredential)
		if err != nil {
			return nil, errors.New("network credential could not be read")
		}
		secret = data
	}
	digest := sha256.Sum256(credential)
	scope := "billing/" + providerID + "/" + hex.EncodeToString(digest[:8])
	cache := i.Connections
	if cache == nil {
		cache = egress.NewConnectionClientCache(64)
	}
	return cache.ClientScoped(scope, *i.Egress, cfg.Options.Network, secret, requestTimout)
}

// loadProviderConfig decodes the provider's stored configuration for the
// endpoint and network options the importer needs.
func loadProviderConfig(ctx context.Context, q access.Queryer, providerID string) (*providerConfig, error) {
	var configuration []byte
	if err := q.QueryRow(ctx, "SELECT configuration FROM olp.providers WHERE id=$1::uuid", providerID).Scan(&configuration); err != nil {
		return nil, err
	}
	var cfg providerConfig
	if err := json.Unmarshal(configuration, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

type sourceRevision struct {
	id         string
	orderingAt time.Time
}

func (i *Importer) storeDays(ctx context.Context, row *importRow, periodStart string, revision *sourceRevision, days map[string]map[lineKey]lineSum) error {
	tx, err := i.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var providerExists int
	if err := tx.QueryRow(ctx, `SELECT 1 FROM olp.providers WHERE id=$1::uuid FOR KEY SHARE`, row.ProviderID).Scan(&providerExists); errors.Is(err, pgx.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	var etag string
	if err := tx.QueryRow(ctx, `SELECT etag FROM olp.provider_billing_imports WHERE provider_id=$1::uuid FOR UPDATE`, row.ProviderID).Scan(&etag); errors.Is(err, pgx.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	if etag != row.ETag {
		return nil
	}
	daysList := slices.Sorted(maps.Keys(days))
	var minDay, maxDay *string
	if len(daysList) > 0 {
		minDay, maxDay = &daysList[0], &daysList[len(daysList)-1]
	}
	var appliedRevision *string
	if revision != nil {
		digest, err := scopeDigest(row.Source, row.Scope)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `INSERT INTO olp.provider_billing_revisions
			(provider_id,source,scope_digest,period_start,source_revision,ordering_at,covered_from,covered_through)
			VALUES($1::uuid,$2,$3,$4::date,$5,$6,$7::date,$8::date)
			ON CONFLICT (provider_id,source,scope_digest,period_start) DO UPDATE SET
				source_revision=EXCLUDED.source_revision,
				ordering_at=CASE WHEN olp.provider_billing_revisions.source_revision=EXCLUDED.source_revision
					THEN olp.provider_billing_revisions.ordering_at ELSE EXCLUDED.ordering_at END,
				covered_from=LEAST(olp.provider_billing_revisions.covered_from,EXCLUDED.covered_from),
				covered_through=GREATEST(olp.provider_billing_revisions.covered_through,EXCLUDED.covered_through)
			WHERE olp.provider_billing_revisions.source_revision=EXCLUDED.source_revision
				OR olp.provider_billing_revisions.ordering_at < EXCLUDED.ordering_at`,
			row.ProviderID, row.Source, digest[:], periodStart, revision.id, revision.orderingAt, minDay, maxDay)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			daysList = nil
			minDay, maxDay = nil, nil
		} else {
			appliedRevision = &revision.id
		}
	}
	for _, day := range daysList {
		if _, err := tx.Exec(ctx, `DELETE FROM olp.provider_invoiced_costs WHERE provider_id=$1::uuid AND day=$2::date`,
			row.ProviderID, day); err != nil {
			return err
		}
		items := days[day]
		keys := slices.SortedFunc(maps.Keys(items), func(a, b lineKey) int {
			if result := cmp.Compare(a.item, b.item); result != 0 {
				return result
			}
			return cmp.Compare(a.currency, b.currency)
		})
		for _, key := range keys {
			sum := items[key]
			if _, err := tx.Exec(ctx, `INSERT INTO olp.provider_invoiced_costs
				(provider_id,day,line_item,currency,amount,source,source_revision) VALUES($1::uuid,$2::date,$3,$4,$5,$6,$7)`,
				row.ProviderID, day, key.item, key.currency, sum.amount.String(), row.Source, appliedRevision); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE olp.provider_billing_imports SET
		last_import_at=now(),last_success_at=now(),last_error_code=NULL,
		consecutive_failures=0,next_import_at=now()+interval '1 hour',updated_at=now(),
		imported_from=LEAST(COALESCE(imported_from,$2::date),$2::date),
		imported_through=GREATEST(COALESCE(imported_through,$3::date),$3::date),
		source_revision=COALESCE($4,source_revision)
		WHERE provider_id=$1::uuid`, row.ProviderID, minDay, maxDay, appliedRevision); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// recordFailure marks one import failed with a content-free code and backs
// its next attempt off exponentially, bounded by an hour.
func (i *Importer) recordFailure(ctx context.Context, row *importRow, code string) error {
	failures := row.ConsecutiveFailures + 1
	delay := baseBackoff
	for n := 1; n < failures && delay < maxBackoff; n++ {
		delay *= 2
	}
	if delay > maxBackoff {
		delay = maxBackoff
	}
	_, err := i.Pool.Exec(ctx, `UPDATE olp.provider_billing_imports SET
		last_import_at=now(),last_error_code=$2,consecutive_failures=$3,
		next_import_at=now()+$4::interval,updated_at=now()
		WHERE provider_id=$1::uuid AND etag=$5`, row.ProviderID, code, failures, delay.String(), row.ETag)
	return err
}

// purge drops invoice rows older than the usage retention window, in bounded
// batches, so the store stays within the same horizon as the spend data.
func (i *Importer) purge(ctx context.Context) error {
	var days int
	if err := i.Pool.QueryRow(ctx, `SELECT COALESCE((SELECT value::int FROM olp.settings WHERE key='retention.usage_days'),90)`).Scan(&days); err != nil {
		return err
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -days).Format(dayFormat)
	for range 20 {
		tag, err := i.Pool.Exec(ctx, `DELETE FROM olp.provider_invoiced_costs WHERE ctid IN (
			SELECT ctid FROM olp.provider_invoiced_costs WHERE day < $1::date LIMIT $2)`, cutoff, purgeBatch)
		if err != nil {
			return err
		}
		if tag.RowsAffected() < purgeBatch {
			return nil
		}
	}
	return nil
}

func scopeDigest(source string, scope json.RawMessage) ([32]byte, error) {
	canonical, err := ValidateScope(source, scope)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(canonical), nil
}

func syncImportRevision(ctx context.Context, tx pgx.Tx, providerID, source string, scope json.RawMessage) error {
	digest, err := scopeDigest(source, scope)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO olp.provider_billing_contexts(provider_id,source,scope)
		VALUES($1::uuid,$2,$3::jsonb) ON CONFLICT (provider_id) DO NOTHING`, providerID, source, scope); err != nil {
		return err
	}
	var previousSource string
	var previousScope json.RawMessage
	if err := tx.QueryRow(ctx, `SELECT source,scope FROM olp.provider_billing_contexts
		WHERE provider_id=$1::uuid FOR UPDATE`, providerID).Scan(&previousSource, &previousScope); err != nil {
		return err
	}
	previousDigest, err := scopeDigest(previousSource, previousScope)
	if err != nil {
		return err
	}
	if previousSource != source || previousDigest != digest {
		if _, err := tx.Exec(ctx, `UPDATE olp.provider_billing_contexts SET source=$2,scope=$3::jsonb
			WHERE provider_id=$1::uuid`, providerID, source, scope); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE olp.provider_billing_revisions SET covered_from=NULL,covered_through=NULL
			WHERE provider_id=$1::uuid`, providerID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE olp.provider_billing_imports SET
			imported_from=NULL,imported_through=NULL,source_revision=NULL WHERE provider_id=$1::uuid`, providerID)
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE olp.provider_billing_imports SET
		imported_from=LEAST(imported_from,(SELECT MIN(covered_from) FROM olp.provider_billing_revisions
			WHERE provider_id=$1::uuid AND source=$2 AND scope_digest=$3)),
		imported_through=GREATEST(imported_through,(SELECT MAX(covered_through) FROM olp.provider_billing_revisions
			WHERE provider_id=$1::uuid AND source=$2 AND scope_digest=$3)),
		source_revision=(SELECT source_revision FROM olp.provider_billing_revisions
			WHERE provider_id=$1::uuid AND source=$2 AND scope_digest=$3 AND covered_from IS NOT NULL
			ORDER BY ordering_at DESC,period_start DESC LIMIT 1)
		WHERE provider_id=$1::uuid`, providerID, source, digest[:])
	return err
}
