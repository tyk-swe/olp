package access

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/tyk-swe/olp/internal/scim"
)

func (s *Server) registerSCIM(mux *http.ServeMux) {
	for _, kind := range []string{"Users", "Groups"} {
		group := kind == "Groups"
		path := "/scim/v2/" + kind
		s.scimRoute(mux, "GET "+path, func(r *http.Request, p Principal) (Reply, error) { return s.scimList(r, p, group) })
		s.scimRoute(mux, "POST "+path+"/.search", func(r *http.Request, p Principal) (Reply, error) { return s.scimList(r, p, group) })
		s.scimRoute(mux, "POST "+path, func(r *http.Request, p Principal) (Reply, error) { return s.scimWrite(r, p, group) })
		s.scimRoute(mux, "GET "+path+"/{scim_id}", func(r *http.Request, p Principal) (Reply, error) { return s.scimGet(r, p, group) })
		for _, method := range []string{"PUT", "PATCH", "DELETE"} {
			s.scimRoute(mux, method+" "+path+"/{scim_id}", func(r *http.Request, p Principal) (Reply, error) { return s.scimWrite(r, p, group) })
		}
	}
	for _, path := range []string{"/scim/v2/ServiceProviderConfig", "/scim/v2/ResourceTypes", "/scim/v2/ResourceTypes/{scim_type}", "/scim/v2/Schemas", "/scim/v2/Schemas/{schema_id}"} {
		s.scimRoute(mux, "GET "+path, s.scimDiscovery)
	}
	mux.HandleFunc("/scim/", func(w http.ResponseWriter, r *http.Request) {
		WriteSCIMError(w, scim.Fail(404, "", "The SCIM operation does not exist."))
	})
}

func (s *Server) scimRoute(mux *http.ServeMux, pattern string, h Handler) {
	req := declared(pattern, false)
	mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		r, cancel, err := s.guard(w, r, scim.MaxBody, 15*time.Second)
		defer cancel()
		var reply Reply
		if err == nil {
			var p Principal
			r, p, err = s.admitRoute(r, req)
			if err == nil {
				if r.Method == "POST" || r.Method == "PUT" || r.Method == "PATCH" {
					typ, _, e := mime.ParseMediaType(r.Header.Get("Content-Type"))
					if e != nil || typ != "application/scim+json" && typ != "application/json" {
						err = scim.Fail(415, "", "Send application/scim+json or application/json.")
					}
				}
				if err == nil {
					reply, err = h(r, p)
				}
			}
		}
		if err != nil {
			WriteSCIMError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/scim+json")
		if reply.ETag != "" {
			w.Header().Set("ETag", `W/"`+reply.ETag+`"`)
		}
		if reply.Location != "" {
			w.Header().Set("Location", reply.Location)
		}
		if reply.Status == 0 {
			reply.Status = 200
		}
		w.WriteHeader(reply.Status)
		if reply.Body != nil {
			_ = json.NewEncoder(w).Encode(reply.Body)
		}
	})
}

func WriteSCIMError(w http.ResponseWriter, err error) {
	status, typ, detail := 503, "", "The SCIM operation is temporarily unavailable."
	if e, ok := errors.AsType[*scim.Error](err); ok {
		status, typ, detail = e.Status, e.Type, e.Detail
	} else if e, ok := errors.AsType[*Problem](err); ok {
		status, detail = e.Status, e.Detail
		if status == 422 {
			status, typ = 400, "invalidValue"
		}
		if status == 412 {
			typ = "invalidVers"
		}
	} else if errors.Is(err, pgx.ErrNoRows) {
		status, detail = 404, "The resource was not found."
	} else if e, ok := errors.AsType[*pgconn.PgError](err); ok && e.Code == "23505" {
		status, typ, detail = 409, "uniqueness", "An identity or group already uses this value."
	} else {
		code := ""
		if pg, ok := errors.AsType[*pgconn.PgError](err); ok {
			code = pg.Code
		}
		slog.Error("SCIM request failed", "error_type", fmt.Sprintf("%T", err), "sqlstate", code)
	}
	body := map[string]any{"schemas": []string{scim.ErrorSchema}, "status": strconv.Itoa(status), "detail": detail}
	if typ != "" {
		body["scimType"] = typ
	}
	if status == 401 {
		w.Header().Set("WWW-Authenticate", `Bearer realm="SCIM"`)
	}
	w.Header().Set("Content-Type", "application/scim+json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
