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
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// maxModuleBytes bounds an uploaded plugin module.
const maxModuleBytes = 32 << 20

// maxInstalled bounds how many plugin digests an installation holds.
const maxInstalled = 64

var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Management serves provider plugin administration on the management API.
// Only an owner with a user session installs, approves, permits and
// uninstalls plugins; any role that may read management state lists them.
type Management struct {
	Access  *access.Server
	Runtime *Runtime
	// Unconfined is the deployment's unconfined tier, or nil where it does
	// not enable one. No API path lists or permits unconfined plugins then.
	Unconfined *Unconfined
}

// Register mounts the plugin operations on the management surface.
func (s *Management) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/plugins", s.Access.Handle(s.list))
	// Instantiating a module to read its manifest can take seconds.
	mux.HandleFunc("POST /api/v1/plugins", s.Access.HandleTimeout(maxModuleBytes, time.Minute, s.install))
	mux.HandleFunc("GET /api/v1/plugins/{plugin_digest}", s.Access.Handle(s.get))
	mux.HandleFunc("POST /api/v1/plugins/{plugin_digest}/approve", s.Access.Handle(s.approve))
	mux.HandleFunc("DELETE /api/v1/plugins/{plugin_digest}", s.Access.Handle(s.uninstall))
	mux.HandleFunc("GET /api/v1/unconfined-plugins", s.Access.Handle(s.executables))
	// Running an executable to read its manifest can take seconds.
	mux.HandleFunc("GET /api/v1/unconfined-plugins/{executable}", s.Access.HandleTimeout(64<<10, time.Minute, s.review))
	mux.HandleFunc("POST /api/v1/unconfined-plugins/{executable}/permit", s.Access.HandleTimeout(64<<10, time.Minute, s.permit))
}

func (s *Management) list(r *http.Request) (access.Reply, error) {
	if _, err := s.Access.Principal(r, s.Access.Pool, "read"); err != nil {
		return access.Reply{}, err
	}
	rows, err := s.Access.Pool.Query(r.Context(), selectPlugin+" WHERE $1 OR p.executable IS NULL ORDER BY p.manifest->>'name', p.installed_at DESC, p.digest", s.Unconfined != nil)
	if err != nil {
		return access.Reply{}, err
	}
	defer rows.Close()
	items := []contract.Plugin{}
	for rows.Next() {
		item, err := scanPlugin(rows)
		if err != nil {
			return access.Reply{}, err
		}
		items = append(items, item)
	}
	return access.OK(contract.PluginListResponse{Items: items, UnconfinedPluginsEnabled: s.Unconfined != nil}), rows.Err()
}

func (s *Management) get(r *http.Request) (access.Reply, error) {
	if _, err := s.Access.Principal(r, s.Access.Pool, "read"); err != nil {
		return access.Reply{}, err
	}
	digest, err := digestParam(r)
	if err != nil {
		return access.Reply{}, err
	}
	plugin, err := s.load(r.Context(), s.Access.Pool, digest)
	return access.Detail(plugin, plugin.Etag.String()), err
}

// load reads an installed plugin that the deployment lets the API show: an
// unconfined plugin only where the unconfined tier is enabled.
func (s *Management) load(ctx context.Context, q access.Queryer, digest string) (contract.Plugin, error) {
	plugin, err := loadPlugin(ctx, q, digest)
	if err == nil && !plugin.Executable.IsNull() && s.Unconfined == nil {
		return contract.Plugin{}, pgx.ErrNoRows
	}
	return plugin, err
}

func (s *Management) install(r *http.Request) (access.Reply, error) {
	// Authorize before reading an upload of up to 32 MiB.
	if _, err := s.Access.OwnerPrincipal(r, s.Access.Pool); err != nil {
		return access.Reply{}, err
	}
	if mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mediaType != "application/wasm" {
		return access.Reply{}, access.Fail(http.StatusUnsupportedMediaType, "unsupported_media_type", "Upload the plugin module as application/wasm.")
	}
	module, err := io.ReadAll(r.Body)
	if _, tooLarge := errors.AsType[*http.MaxBytesError](err); tooLarge {
		return access.Reply{}, access.Fail(http.StatusRequestEntityTooLarge, "plugin_too_large", "A plugin module may be at most 32 MiB.")
	}
	if err != nil {
		return access.Reply{}, err
	}
	sum := sha256.Sum256(module)
	digest := hex.EncodeToString(sum[:])
	if installed, err := loadPlugin(r.Context(), s.Access.Pool, digest); !errors.Is(err, pgx.ErrNoRows) {
		return access.Detail(installed, installed.Etag.String()), err
	}
	manifest, err := s.Runtime.Inspect(r.Context(), module)
	if err != nil {
		return access.Reply{}, problem(err)
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		return access.Reply{}, err
	}
	tx, err := s.Access.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := s.Access.OwnerPrincipal(r, tx)
	if err != nil {
		return access.Reply{}, err
	}
	var count int
	if err = tx.QueryRow(r.Context(), "SELECT count(*) FROM olp.plugins").Scan(&count); err != nil {
		return access.Reply{}, err
	}
	if count >= maxInstalled {
		return access.Reply{}, access.Fail(http.StatusUnprocessableEntity, "plugin_limit", "An installation holds at most 64 plugin digests. Uninstall one first.")
	}
	tag, err := tx.Exec(r.Context(), "INSERT INTO olp.plugins(digest,abi_version,manifest,module,size_bytes,etag,installed_by) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT (digest) DO NOTHING",
		digest, abi.Version, data, module, len(module), access.NewID(), p.ID)
	if err != nil {
		return access.Reply{}, err
	}
	status := http.StatusOK
	if tag.RowsAffected() == 1 {
		status = http.StatusCreated
		if err = access.Audit(r.Context(), tx, r, p.ID, "plugin.install", "plugin", digest, "success"); err != nil {
			return access.Reply{}, err
		}
	}
	installed, err := loadPlugin(r.Context(), tx, digest)
	if err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, access.Reply{Status: status, Body: installed, ETag: installed.Etag.String(), Location: "/api/v1/plugins/" + digest})
}

func (s *Management) approve(r *http.Request) (access.Reply, error) {
	digest, err := digestParam(r)
	if err != nil {
		return access.Reply{}, err
	}
	var input contract.PluginApprovalRequest
	if err = access.Decode(r, &input); err != nil {
		return access.Reply{}, err
	}
	tx, err := s.Access.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := s.Access.OwnerPrincipal(r, tx)
	if err != nil {
		return access.Reply{}, err
	}
	plugin, err := s.load(r.Context(), tx, digest)
	if err != nil {
		return access.Reply{}, err
	}
	if err = access.Match(r, plugin.Etag.String()); err != nil {
		return access.Reply{}, err
	}
	if !plugin.ApprovedAt.IsNull() {
		return access.Reply{}, access.Fail(http.StatusConflict, "plugin_approved", "An owner already approved this plugin.")
	}
	if !slices.Equal(slices.Sorted(slices.Values(input.Origins)), slices.Sorted(slices.Values(plugin.Manifest.Origins))) {
		return access.Reply{}, &access.Problem{Status: http.StatusUnprocessableEntity, Code: "plugin_origins_mismatch", Field: "origins",
			Detail: "Approve exactly the origins the plugin declares; review them again."}
	}
	if _, err = tx.Exec(r.Context(), "UPDATE olp.plugins SET approved_by=$1, approved_at=now(), etag=$2 WHERE digest=$3", p.ID, access.NewID(), digest); err != nil {
		return access.Reply{}, err
	}
	if err = access.Audit(r.Context(), tx, r, p.ID, "plugin.approve", "plugin", digest, "success"); err != nil {
		return access.Reply{}, err
	}
	if plugin, err = loadPlugin(r.Context(), tx, digest); err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, access.Detail(plugin, plugin.Etag.String()))
}

func (s *Management) uninstall(r *http.Request) (access.Reply, error) {
	digest, err := digestParam(r)
	if err != nil {
		return access.Reply{}, err
	}
	tx, err := s.Access.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := s.Access.OwnerPrincipal(r, tx)
	if err != nil {
		return access.Reply{}, err
	}
	plugin, err := s.load(r.Context(), tx, digest)
	if err != nil {
		return access.Reply{}, err
	}
	etag := plugin.Etag.String()
	if err = access.Match(r, etag); err != nil {
		return access.Reply{}, err
	}
	// Provider writes hold the same installation lock, so no revision can pin
	// the plugin between this check and the delete.
	pinning, err := pinningProviders(r.Context(), tx, digest)
	if err != nil {
		return access.Reply{}, err
	}
	if len(pinning) > 0 {
		return access.Reply{}, access.Fail(http.StatusConflict, CodePinned, "A draft or published revision of "+pinning+" pins this plugin. Move those providers to another plugin before uninstalling it.")
	}
	if _, err = tx.Exec(r.Context(), "DELETE FROM olp.plugins WHERE digest=$1", digest); err != nil {
		return access.Reply{}, err
	}
	if err = access.Audit(r.Context(), tx, r, p.ID, "plugin.uninstall", "plugin", digest, "success"); err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, access.Reply{Status: http.StatusNoContent})
}

// pinningProviders names the providers whose draft or any published
// revision pins a plugin digest as its profile revision, or returns "".
func pinningProviders(ctx context.Context, q access.Queryer, digest string) (string, error) {
	rows, err := q.Query(ctx, `SELECT p.name FROM olp.providers p
		WHERE p.kind='plugin' AND p.configuration->>'profile_revision'=$1
		   OR EXISTS (SELECT 1 FROM olp.provider_revisions r WHERE r.provider_id=p.id AND r.configuration->>'kind'='plugin' AND r.configuration->>'profile_revision'=$1)
		ORDER BY lower(p.name), p.id`, digest)
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
		listed += fmt.Sprintf(" and %d more providers", len(names)-shown)
	}
	return listed, nil
}

// problem is the problem a refusal of a plugin becomes.
func problem(err error) error {
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
	return &access.Problem{Status: status, Code: refusal.Code, Detail: refusal.Message, Field: refusal.Field}
}

func digestParam(r *http.Request) (string, error) {
	digest := r.PathValue("plugin_digest")
	if !digestPattern.MatchString(digest) {
		return "", access.Fail(http.StatusBadRequest, "invalid_identifier", "Identify the plugin by the lowercase hexadecimal SHA-256 digest of its module.")
	}
	return digest, nil
}
