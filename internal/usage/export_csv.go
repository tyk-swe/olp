package usage

import (
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
)

func csvHeaders(w http.ResponseWriter, name string) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-store")
}

func (s *Server) exportRequestsCSV(w http.ResponseWriter, r *http.Request, p access.Principal) error {
	filters, err := s.requestFilters(r, p)
	if err != nil {
		return err
	}
	tx, err := s.Access.Pool.BeginTx(r.Context(), pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	items, err := ReadRequestExport(r.Context(), tx, filters)
	if rollbackErr := tx.Rollback(r.Context()); rollbackErr != nil && err == nil {
		err = rollbackErr
	}
	if err != nil {
		return err
	}
	data, err := RequestCSV(items)
	if err != nil {
		return err
	}
	csvHeaders(w, "olp-requests.csv")
	w.WriteHeader(http.StatusOK)
	_, err = w.Write(data)
	return err
}

func (s *Server) exportUsageCSV(w http.ResponseWriter, r *http.Request, p access.Principal) error {
	filters, err := usageFilters(r, p)
	if err != nil {
		return err
	}
	dimension := strings.TrimSpace(r.URL.Query().Get("dimension"))
	if dimension == "" {
		dimension = DimensionRoute
	}
	tx, err := s.Access.Pool.BeginTx(r.Context(), pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	summary, err := ReadSummary(r.Context(), tx, filters, time.Now())
	if err != nil {
		tx.Rollback(r.Context())
		return err
	}
	breakdown, err := ReadBreakdownExport(r.Context(), tx, filters, dimension)
	if rollbackErr := tx.Rollback(r.Context()); rollbackErr != nil && err == nil {
		err = rollbackErr
	}
	if err != nil {
		return err
	}
	data, err := UsageCSV(filters, dimension, summary, breakdown)
	if err != nil {
		return err
	}
	csvHeaders(w, "olp-usage.csv")
	w.WriteHeader(http.StatusOK)
	_, err = w.Write(data)
	return err
}
