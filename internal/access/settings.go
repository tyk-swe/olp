package access

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/tyk-swe/olp/internal/access/httpkit"
)

const settingJSON = `to_jsonb(s)||CASE WHEN s.key='budgets.time_zone' THEN jsonb_build_object('calendar',olp.budget_calendar_status(now())) ELSE '{}'::jsonb END`

func (s *Server) settings(r *http.Request, p Principal) (httpkit.Reply, error) {
	var err error
	rows, err := s.Pool.Query(r.Context(), "SELECT "+settingJSON+" FROM olp.settings s ORDER BY key")
	if err != nil {
		return httpkit.Reply{}, err
	}
	items, err := httpkit.JSONRows(rows)
	return httpkit.OK(map[string]any{"items": items}), err
}

func (s *Server) setting(r *http.Request, p Principal) (httpkit.Reply, error) {
	var err error
	var data []byte
	var etag string
	err = s.Pool.QueryRow(r.Context(), "SELECT "+settingJSON+",etag::text FROM olp.settings s WHERE key=$1", r.PathValue("key")).Scan(&data, &etag)
	return httpkit.Detail(json.RawMessage(data), etag), err
}

func (s *Server) updateSetting(r *http.Request, _ Principal) (httpkit.Reply, error) {
	var input struct {
		Value string `json:"value"`
	}
	if err := httpkit.Decode(r, &input); err != nil {
		return httpkit.Reply{}, err
	}
	key := r.PathValue("key")
	switch key {
	case "retention.requests_days", "retention.usage_days", "retention.audit_days":
		n, err := strconv.Atoi(input.Value)
		if err != nil || n < 1 || n > 3650 {
			return httpkit.Reply{}, httpkit.Invalid("value", "Use an integer from 1 to 3650.")
		}
	case "guardrails.stream_window_bytes":
		n, err := strconv.Atoi(input.Value)
		if err != nil || n < 256 || n > 1<<20 {
			return httpkit.Reply{}, httpkit.Invalid("value", "Use a byte window between 256 and 1048576.")
		}
	case "budgets.time_zone":
		if err := ValidateBudgetTimeZone(input.Value); err != nil {
			return httpkit.Reply{}, httpkit.Invalid("value", "Use a supported IANA time zone.")
		}
	case "limits.valkey_unavailable":
		if input.Value != "fail_open" && input.Value != "fail_closed" {
			return httpkit.Reply{}, httpkit.Invalid("value", "Use fail_open or fail_closed.")
		}
	case "auth.local_login_enabled", "auth.mfa_required":
		if input.Value != "true" && input.Value != "false" {
			return httpkit.Reply{}, httpkit.Invalid("value", "Use true or false.")
		}
	default:
		return httpkit.Reply{}, httpkit.Fail(404, "not_found", "This setting does not exist.")
	}
	tx, p, err := s.BeginMutation(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	if key == "auth.local_login_enabled" {
		if err = p.Authorize(LocalLogin); err != nil {
			return httpkit.Reply{}, err
		}
	}
	if key == "auth.mfa_required" {
		if err = p.Authorize(Access); err != nil {
			return httpkit.Reply{}, err
		}
	}
	var etag string
	if err = tx.QueryRow(r.Context(), "SELECT etag::text FROM olp.settings WHERE key=$1", key).Scan(&etag); err != nil {
		return httpkit.Reply{}, err
	}
	if err = httpkit.Match(r, etag); err != nil {
		return httpkit.Reply{}, err
	}
	if key == "budgets.time_zone" {
		if _, err = tx.Exec(r.Context(), "SELECT olp.schedule_budget_zone($1,now())", input.Value); err != nil {
			return httpkit.Reply{}, err
		}
	}
	etag = httpkit.NewID()
	if _, err = tx.Exec(r.Context(), "UPDATE olp.settings SET value=$1,etag=$2,updated_by=$3,updated_at=now() WHERE key=$4", input.Value, etag, p.UserID(), key); err != nil {
		return httpkit.Reply{}, err
	}
	if key == "auth.local_login_enabled" {
		if err = s.EnsureUsableOwner(r.Context(), tx); err != nil {
			return httpkit.Reply{}, err
		}
	}
	if key == "guardrails.stream_window_bytes" && s.RuntimePublisher != nil {
		if err = s.RuntimePublisher(r.Context(), tx, p.UserID()); err != nil {
			return httpkit.Reply{}, httpkit.Invalid("value", "The window is incompatible with published guardrails.")
		}
	}
	if err = Audit(r.Context(), tx, r, p.Actor(), "setting.update", "setting", key, "success"); err != nil {
		return httpkit.Reply{}, err
	}
	var data []byte
	if err = tx.QueryRow(r.Context(), "SELECT "+settingJSON+" FROM olp.settings s WHERE key=$1", key).Scan(&data); err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.Detail(json.RawMessage(data), etag), tx.Commit(r.Context())
}
