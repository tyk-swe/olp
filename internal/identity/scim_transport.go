package identity

import (
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/identity/scim"
)

type scimErrorResponse struct {
	Schemas  []string `json:"schemas"`
	Status   string   `json:"status"`
	Detail   string   `json:"detail"`
	SCIMType string   `json:"scimType,omitempty"`
}

func (s *scimService) register(mux *http.ServeMux) {
	s.access.Route(mux, "GET /api/v1/scim/groups", s.scimGroups)
	s.access.Route(mux, "PUT /api/v1/scim/groups/{scim_id}/mapping", s.scimGroupMapping)
	for _, kind := range []string{"Users", "Groups"} {
		group := kind == "Groups"
		path := "/scim/v2/" + kind
		s.scimRoute(mux, "GET "+path, func(r *http.Request, p authz.Principal) (httpkit.Reply, error) { return s.scimList(r, p, group) })
		s.scimRoute(mux, "POST "+path+"/.search", func(r *http.Request, p authz.Principal) (httpkit.Reply, error) { return s.scimList(r, p, group) })
		s.scimRoute(mux, "POST "+path, func(r *http.Request, p authz.Principal) (httpkit.Reply, error) { return s.scimWrite(r, p, group) })
		s.scimRoute(mux, "GET "+path+"/{scim_id}", func(r *http.Request, p authz.Principal) (httpkit.Reply, error) { return s.scimGet(r, p, group) })
		for _, method := range []string{"PUT", "PATCH", "DELETE"} {
			s.scimRoute(mux, method+" "+path+"/{scim_id}", func(r *http.Request, p authz.Principal) (httpkit.Reply, error) { return s.scimWrite(r, p, group) })
		}
	}
	for _, path := range []string{"/scim/v2/ServiceProviderConfig", "/scim/v2/ResourceTypes", "/scim/v2/ResourceTypes/{scim_type}", "/scim/v2/Schemas", "/scim/v2/Schemas/{schema_id}"} {
		s.scimRoute(mux, "GET "+path, s.scimDiscovery)
	}
	mux.HandleFunc("/scim/", func(w http.ResponseWriter, _ *http.Request) {
		WriteSCIMError(w, scim.Fail(404, "", "The SCIM operation does not exist."))
	})
}

func (s *scimService) scimRoute(mux *http.ServeMux, pattern string, h access.Handler) {
	admit := s.access.Admit(pattern, access.MaxBody(scim.MaxBody), access.AnyOrigin())
	mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		req, p, cancel, err := admit(w, r)
		defer cancel()
		var reply httpkit.Reply
		if err == nil && (req.Method == "POST" || req.Method == "PUT" || req.Method == "PATCH") {
			typ, _, mediaErr := mime.ParseMediaType(req.Header.Get("Content-Type"))
			if mediaErr != nil || typ != "application/scim+json" && typ != "application/json" {
				err = scim.Fail(415, "", "Send application/scim+json or application/json.")
			}
		}
		if err == nil {
			reply, err = h(req, p)
		}
		if err != nil {
			s.writeSCIMError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/scim+json")
		if reply.ETag != "" {
			w.Header().Set("ETag", `W/"`+reply.ETag+`"`)
		}
		if reply.Location != "" {
			w.Header().Set("Location", reply.Location)
		}
		status := reply.Status
		if status == 0 {
			status = 200
		}
		w.WriteHeader(status)
		if reply.Body != nil {
			_ = json.NewEncoder(w).Encode(reply.Body)
		}
	})
}

type scimFailure struct {
	status      int
	typ, detail string
}

func (s *scimService) writeSCIMError(w http.ResponseWriter, err error) {
	f, known := scimFailureFor(err)
	if !known {
		s.access.LogFailure("SCIM request failed", err)
	}
	renderSCIMError(w, f)
}

func WriteSCIMError(w http.ResponseWriter, err error) {
	f, _ := scimFailureFor(err)
	renderSCIMError(w, f)
}

func scimFailureFor(err error) (scimFailure, bool) {
	if e, ok := errors.AsType[*scim.Error](err); ok {
		return scimFailure{e.Status, e.Type, e.Detail}, true
	}
	if e, ok := errors.AsType[*httpkit.Problem](err); ok {
		switch e.Status {
		case 422:
			return scimFailure{400, "invalidValue", e.Detail}, true
		case 412:
			return scimFailure{412, "invalidVers", e.Detail}, true
		}
		return scimFailure{e.Status, "", e.Detail}, true
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return scimFailure{404, "", "The resource was not found."}, true
	}
	if e, ok := errors.AsType[*pgconn.PgError](err); ok && e.Code == "23505" {
		return scimFailure{409, "uniqueness", "An identity or group already uses this value."}, true
	}
	return scimFailure{503, "", "The SCIM operation is temporarily unavailable."}, false
}

func renderSCIMError(w http.ResponseWriter, f scimFailure) {
	body := scimErrorResponse{
		Schemas:  []string{scim.ErrorSchema},
		Status:   strconv.Itoa(f.status),
		Detail:   f.detail,
		SCIMType: f.typ,
	}
	if f.status == 401 {
		w.Header().Set("WWW-Authenticate", `Bearer realm="SCIM"`)
	}
	w.Header().Set("Content-Type", "application/scim+json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(f.status)
	_ = json.NewEncoder(w).Encode(body)
}
