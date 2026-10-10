//go:build integration

package integration_test

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/png"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

func TestInstallationBrandingUsesAuthorityETagsAndSessionIdentity(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	viewer := h.invite(owner, "branding-viewer@example.com", "viewer")
	identity := h.want(owner, "GET", "/api/v1/branding", nil, nil, 200)
	h.want(viewer, "PUT", "/api/v1/branding", map[string]any{"name": "Unauthorized", "logo": ""}, etagHeader(identity), 403)
	h.want(owner, "PUT", "/api/v1/branding", map[string]any{"name": "Operator fleet", "logo": ""}, nil, 428)
	h.want(owner, "PUT", "/api/v1/branding", map[string]any{"name": "Operator fleet", "logo": "data:image/svg+xml;base64,PHN2Zz4="}, etagHeader(identity), 422)
	h.want(owner, "PUT", "/api/v1/branding", map[string]any{"name": "Operator fleet"}, etagHeader(identity), 422)
	var data bytes.Buffer
	png.Encode(&data, image.NewNRGBA(image.Rect(0, 0, 22, 22)))
	logo := "data:image/png;base64," + base64.StdEncoding.EncodeToString(data.Bytes())
	updated := h.want(owner, "PUT", "/api/v1/branding", map[string]any{"name": " Operator fleet ", "logo": logo}, etagHeader(identity), 200)
	if updated["name"] != "Operator fleet" || updated["logo"] != logo || updated["etag"] == identity["etag"] {
		t.Fatalf("branding did not update atomically: %v", updated)
	}
	h.want(owner, "PUT", "/api/v1/branding", map[string]any{"name": "Stale write", "logo": ""}, etagHeader(identity), 412)
	session := h.want(owner, "GET", "/api/v1/sessions/current", nil, nil, http.StatusOK)
	if session["installation_name"] != updated["name"] || session["installation_logo"] != logo {
		t.Fatal("session omitted installation branding")
	}
	if _, err := uuid.Parse(session["session_id"].(string)); err != nil {
		t.Fatalf("session identifier: %v", err)
	}
	var audited int
	if err := h.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp.audit WHERE action='branding.update'").Scan(&audited); err != nil || audited != 1 {
		t.Fatalf("branding audit=%d error=%v", audited, err)
	}
}
