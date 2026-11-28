package plugins

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/domain/ids"
	pluginidentity "github.com/tyk-swe/olp/internal/domain/pluginidentity"
	"github.com/tyk-swe/olp/internal/domain/problem"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/platform/database"
	"github.com/tyk-swe/olp/internal/platform/secrets"
	"github.com/tyk-swe/olp/internal/plugins/pluginindex"
	"github.com/tyk-swe/olp/plugin/abi"
)

// maxModuleBytes bounds an uploaded plugin module.
const maxModuleBytes = 32 << 20

// maxInstalled bounds how many plugin digests an installation holds.
const maxInstalled = 64

// Management serves provider plugin administration on the management API.
// Only an owner with a user session installs, approves, permits and
// uninstalls plugins; any role that may read management state lists them.
type Management struct {
	Access  *access.Server
	Runtime *Runtime
	// Host runs installed plugins' code for this process. Approving a plugin
	// prepares its code there, as providers are about to use it.
	Host *Host
	// Unconfined is the deployment's unconfined tier, or nil where it does
	// not enable one. No API path lists or permits unconfined plugins then.
	Unconfined *Unconfined
	// Index is the signed index of reviewed plugins this release ships,
	// verified at start-up.
	Index *pluginindex.Signed
}

// Register mounts the plugin operations on the management surface.
func (s *Management) Register(mux *http.ServeMux) {
	s.Access.Route(mux, "GET /api/v1/plugins", s.list)
	s.Access.Route(mux, "GET /api/v1/plugin-index", s.index)
	// Instantiating a module to read its manifest can take seconds.
	s.Access.Route(mux, "POST /api/v1/plugins", s.install, access.MaxBody(maxModuleBytes), access.Deadline(time.Minute))
	s.Access.Route(mux, "GET /api/v1/plugins/{plugin_digest}", s.get)
	s.Access.Route(mux, "POST /api/v1/plugins/{plugin_digest}/approve", s.approve)
	s.Access.Route(mux, "DELETE /api/v1/plugins/{plugin_digest}", s.uninstall)
	s.Access.Route(mux, "GET /api/v1/unconfined-plugins", s.executables)
	// Running an executable to read its manifest can take seconds, and runs
	// only for a caller that proved the session's CSRF token.
	s.Access.Route(mux, "POST /api/v1/unconfined-plugins/{executable}/review", s.review, access.MaxBody(64<<10), access.Deadline(time.Minute))
	s.Access.Route(mux, "POST /api/v1/unconfined-plugins/{executable}/permit", s.permit, access.MaxBody(64<<10), access.Deadline(time.Minute))
}

func (s *Management) list(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	rows, err := s.Access.Pool.Query(r.Context(), selectPlugin+" WHERE $1 OR p.executable IS NULL ORDER BY p.manifest->>'name', p.installed_at DESC, p.digest", s.Unconfined != nil)
	if err != nil {
		return httpkit.Reply{}, err
	}
	defer rows.Close()
	items := []contract.Plugin{}
	for rows.Next() {
		item, err := scanPlugin(rows)
		if err != nil {
			return httpkit.Reply{}, err
		}
		items = append(items, item)
	}
	return httpkit.OK(contract.PluginListResponse{Items: items, UnconfinedPluginsEnabled: s.Unconfined != nil}), rows.Err()
}

func (s *Management) get(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	digest, err := digestParam(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	plugin, err := s.load(r.Context(), s.Access.Pool, digest)
	return httpkit.Detail(plugin, plugin.Etag.String()), err
}

// load reads an installed plugin that the deployment lets the API show: an
// unconfined plugin only where the unconfined tier is enabled.
func (s *Management) load(ctx context.Context, q database.Queryer, digest string) (contract.Plugin, error) {
	plugin, err := loadPlugin(ctx, q, digest)
	if err == nil && !plugin.Executable.IsNull() && s.Unconfined == nil {
		return contract.Plugin{}, pgx.ErrNoRows
	}
	return plugin, err
}

func (s *Management) install(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	// The route authorizes before reading an upload of up to 32 MiB.
	if mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mediaType != "application/wasm" {
		return httpkit.Reply{}, problem.Fail(http.StatusUnsupportedMediaType, "unsupported_media_type", "Upload the plugin module as application/wasm.")
	}
	module, err := io.ReadAll(r.Body)
	if _, tooLarge := errors.AsType[*http.MaxBytesError](err); tooLarge {
		return httpkit.Reply{}, problem.Fail(http.StatusRequestEntityTooLarge, "plugin_too_large", "A plugin module may be at most 32 MiB.")
	}
	if err != nil {
		return httpkit.Reply{}, err
	}
	sum := sha256.Sum256(module)
	digest := hex.EncodeToString(sum[:])
	if installed, err := loadPlugin(r.Context(), s.Access.Pool, digest); !errors.Is(err, pgx.ErrNoRows) {
		return httpkit.Detail(installed, installed.Etag.String()), err
	}
	manifest, err := s.Runtime.Inspect(r.Context(), module)
	if err != nil {
		return httpkit.Reply{}, pluginProblem(err)
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		return httpkit.Reply{}, err
	}
	return access.Transact(s.Access, r, access.Unlocked, func(tx pgx.Tx, p authz.Principal, _ struct{}) (httpkit.Reply, access.Event, error) {
		var count int
		if err := tx.QueryRow(r.Context(), "SELECT count(*) FROM olp.plugins").Scan(&count); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if count >= maxInstalled {
			return httpkit.Reply{}, access.Event{}, problem.Fail(http.StatusUnprocessableEntity, "plugin_limit", "An installation holds at most 64 plugin digests. Uninstall one first.")
		}
		tag, err := tx.Exec(r.Context(), "INSERT INTO olp.plugins(digest,abi_version,manifest,module,size_bytes,etag,installed_by) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT (digest) DO NOTHING",
			digest, abi.Version, data, module, len(module), ids.New(), p.ID)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		installed, err := loadPlugin(r.Context(), tx, digest)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		reply := httpkit.Reply{Status: http.StatusOK, Body: installed, ETag: installed.Etag.String(), Location: "/api/v1/plugins/" + digest}
		if tag.RowsAffected() == 0 {
			return reply, access.Event{}, nil
		}
		reply.Status = http.StatusCreated
		return reply, access.Event{Action: "plugin.install", Resource: "plugin", ID: digest}, nil
	})
}

func (s *Management) approve(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	digest, err := digestParam(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	var input contract.PluginApprovalRequest
	if err = httpkit.Decode(r, &input); err != nil {
		return httpkit.Reply{}, err
	}
	reply, err := access.Transact(s.Access, r, s.matched(r, digest), func(tx pgx.Tx, p authz.Principal, plugin contract.Plugin) (httpkit.Reply, access.Event, error) {
		if !plugin.ApprovedAt.IsNull() {
			return httpkit.Reply{}, access.Event{}, problem.Fail(http.StatusConflict, "plugin_approved", "An owner already approved this plugin.")
		}
		if !slices.Equal(slices.Sorted(slices.Values(input.Origins)), slices.Sorted(slices.Values(plugin.Manifest.Origins))) {
			return httpkit.Reply{}, access.Event{}, &problem.Problem{Status: http.StatusUnprocessableEntity, Code: "plugin_origins_mismatch", Field: "origins",
				Detail: "Approve exactly the origins the plugin declares; review them again."}
		}
		if _, err := tx.Exec(r.Context(), "UPDATE olp.plugins SET approved_by=$1, approved_at=now(), etag=$2 WHERE digest=$3", p.ID, ids.New(), digest); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		approved, err := loadPlugin(r.Context(), tx, digest)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		return httpkit.Detail(approved, approved.Etag.String()), access.Event{Action: "plugin.approve", Resource: "plugin", ID: digest}, nil
	})
	if err == nil {
		s.Host.Prepare(digest)
	}
	return reply, err
}

func (s *Management) uninstall(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	digest, err := digestParam(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	reply, err := access.Transact(s.Access, r, s.matched(r, digest), func(tx pgx.Tx, p authz.Principal, _ contract.Plugin) (httpkit.Reply, access.Event, error) {
		return s.remove(r, tx, p, digest)
	})
	if err == nil {
		// The plugin's cached code here stops; every other replica's copy is
		// evicted when its next usability recheck observes the removal.
		s.Host.Evict(digest)
	}
	return reply, err
}

func (s *Management) matched(r *http.Request, digest string) func(pgx.Tx, authz.Principal) (contract.Plugin, error) {
	return func(tx pgx.Tx, _ authz.Principal) (contract.Plugin, error) {
		plugin, err := s.load(r.Context(), tx, digest)
		if err != nil {
			return contract.Plugin{}, err
		}
		return plugin, httpkit.Match(r, plugin.Etag.String())
	}
}

func (s *Management) remove(r *http.Request, tx pgx.Tx, p authz.Principal, digest string) (httpkit.Reply, access.Event, error) {
	// Configuration writes hold the same installation lock, so no revision can pin
	// the plugin between this check and the delete.
	pinning, err := pinningResources(r.Context(), tx, digest)
	if err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	if len(pinning) > 0 {
		return httpkit.Reply{}, access.Event{}, problem.Fail(http.StatusConflict, CodePinned, "A draft or published revision of "+pinning+" pins this plugin. Keep this plugin installed while those revisions exist.")
	}
	retired, err := retireGrants(r.Context(), tx, digest)
	if err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	for _, credentialID := range retired {
		if err = access.Audit(r.Context(), tx, r, p.Actor(), "provider.grant.retire", "provider_credential", credentialID, "success"); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
	}
	if len(retired) > 0 {
		if _, err = access.AdvanceAuthority(r.Context(), tx); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
	}
	if _, err = tx.Exec(r.Context(), "DELETE FROM olp.plugins WHERE digest=$1", digest); err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	return httpkit.Reply{Status: http.StatusNoContent}, access.Event{Action: "plugin.uninstall", Resource: "plugin", ID: digest}, nil
}

// retireGrants retires, as a plugin build is uninstalled, the unrevoked
// grants its grant enrollment created that have not lapsed, and returns their
// credential versions. No draft or revision pins the build any more, and a
// grant serves only a provider pinning the build that enrolled it, so nothing
// can use them: like a worker retiring a grant no configuration uses (see
// package grants), this deletes their refresh tokens and lapses them, without
// notifying anyone, since nothing served them. A restored revision, or a
// reinstalled build, serves their credential versions only after a new grant
// enrollment.
func retireGrants(ctx context.Context, tx pgx.Tx, digest string) ([]string, error) {
	var retired, refreshTokens []string
	err := tx.QueryRow(ctx, `WITH retired AS (
		UPDATE olp.provider_grants g SET lapsed_at=now(),refresh_token_id=NULL,refresh_at=NULL,refresh_attempt_id=NULL,updated_at=now()
		FROM olp.provider_credentials c WHERE c.id=g.credential_id AND c.plugin_digest=$1 AND c.revoked_at IS NULL AND g.lapsed_at IS NULL
		RETURNING g.credential_id::text credential_id, old.refresh_token_id::text refresh_token_id
		) SELECT array_agg(credential_id), array_remove(array_agg(refresh_token_id), NULL) FROM retired`, digest).Scan(&retired, &refreshTokens)
	if err != nil || len(refreshTokens) == 0 {
		return retired, err
	}
	_, err = tx.Exec(ctx, "DELETE FROM olp.secrets WHERE id=ANY($1::uuid[]) AND purpose=$2", refreshTokens, secrets.ProviderGrantRefresh)
	return retired, err
}

// pinningResources names resources whose drafts or immutable revisions pin a plugin digest.
func pinningResources(ctx context.Context, q database.Queryer, digest string) (string, error) {
	rows, err := q.Query(ctx, `SELECT name FROM (SELECT p.name, p.id FROM olp.providers p
		WHERE p.kind='plugin' AND p.configuration->>'profile_revision'=$1
		   OR EXISTS (SELECT 1 FROM olp.provider_revisions r WHERE r.provider_id=p.id AND r.configuration->>'kind'='plugin' AND r.configuration->>'profile_revision'=$1)
		   OR EXISTS (SELECT 1 FROM olp.code_route_revisions r WHERE r.connections->p.id::text->>'kind'='plugin' AND r.connections->p.id::text->>'profile_revision'=$1)
		UNION SELECT g.name, g.id FROM olp.guardrails g
		JOIN olp.guardrail_revisions r ON r.guardrail_id=g.id
		WHERE g.type='plugin' AND r.configuration->>'plugin_version'=$1
		UNION SELECT d.slug, d.id FROM olp.route_drafts d
		WHERE EXISTS (SELECT 1 FROM jsonb_array_elements(d.behavior->'selectors') selector WHERE selector #>> '{when,plugin,digest}'=$1)
		UNION SELECT r.slug, r.id FROM olp.route_revisions r
		WHERE EXISTS (SELECT 1 FROM jsonb_array_elements(r.behavior->'selectors') selector WHERE selector #>> '{when,plugin,digest}'=$1)
		) pins ORDER BY lower(name), id`, digest)
	if err != nil {
		return "", err
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(names) == 0 {
		return "", err
	}
	const shown = 10
	quoted := make([]string, 0, shown)
	for _, name := range names[:min(len(names), shown)] {
		quoted = append(quoted, strconv.Quote(name))
	}
	listed := strings.Join(quoted, ", ")
	if len(names) > shown {
		listed += fmt.Sprintf(" and %d more resources", len(names)-shown)
	}
	return listed, nil
}

// pluginProblem is the problem a refusal of a plugin becomes.
func pluginProblem(err error) error {
	refusal, ok := errors.AsType[*Error](err)
	if !ok {
		return err
	}
	status := http.StatusUnprocessableEntity
	switch refusal.Code {
	case CodeExecutableUnknown:
		status = http.StatusNotFound
	case CodeExecutableChanged:
		status = http.StatusConflict
	}
	return &problem.Problem{Status: status, Code: refusal.Code, Detail: refusal.Message, Field: refusal.Field}
}

func digestParam(r *http.Request) (string, error) {
	digest := r.PathValue("plugin_digest")
	if !pluginidentity.ValidDigest(digest) {
		return "", problem.Fail(http.StatusBadRequest, "invalid_identifier", "Identify the plugin by the lowercase hexadecimal SHA-256 digest of its module.")
	}
	return digest, nil
}
