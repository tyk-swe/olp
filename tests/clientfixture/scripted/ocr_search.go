package scripted

import (
	"net/http"
)

// Operation vendors: each is a prefix of its own so a provider endpoint
// address resolves exactly where the real API lives.
const (
	MistralPrefix    = "/mistral/v1"
	PerplexityPrefix = "/perplexity"
	TavilyPrefix     = "/tavily"
	ExaPrefix        = "/exa"
	ParallelPrefix   = "/parallel/v1"

	MistralModel    = "fixture-mistral"
	PerplexityModel = "fixture-perplexity"
	TavilyModel     = "fixture-tavily"
	ExaModel        = "fixture-exa"
	ParallelModel   = "fixture-parallel"
)

func bearer(name string) vendor {
	return vendor{
		name: name,
		authorized: func(r *http.Request, credential string) bool {
			return r.Header.Get("Authorization") == "Bearer "+credential
		},
		fail: failDetail,
	}
}

var (
	mistral    = bearer("mistral")
	perplexity = bearer("perplexity")
	tavily     = bearer("tavily")
	parallel   = vendor{
		name: "parallel",
		authorized: func(r *http.Request, credential string) bool {
			return r.Header.Get("X-Api-Key") == credential
		},
		fail: func(w http.ResponseWriter, status int, kind, message string) {
			writeJSON(w, status, map[string]any{"type": "error", "error": map[string]any{"ref_id": "ref_fx", "message": message}})
		},
	}
	exa = vendor{
		name: "exa",
		authorized: func(r *http.Request, credential string) bool {
			return r.Header.Get("Authorization") == "Bearer "+credential
		},
		fail: func(w http.ResponseWriter, status int, kind, message string) {
			writeJSON(w, status, map[string]any{"requestId": "exa_fx", "error": message, "tag": kind})
		},
	}
)

// failDetail answers in the FastAPI validation envelope Mistral, Perplexity
// and Tavily share.
func failDetail(w http.ResponseWriter, status int, kind, message string) {
	writeJSON(w, status, map[string]any{"detail": []any{map[string]any{"loc": []any{"body"}, "msg": message, "type": kind}}})
}

func (f *Fixture) registerOperations() {
	f.mux.HandleFunc("POST "+MistralPrefix+"/ocr", f.handle(mistral, "mistral.ocr", f.mistralOCR))
	f.mux.HandleFunc("POST "+PerplexityPrefix+"/search", f.handle(perplexity, "perplexity.search", f.perplexitySearch))
	f.mux.HandleFunc("POST "+TavilyPrefix+"/search", f.handle(tavily, "tavily.search", f.tavilySearch))
	f.mux.HandleFunc("POST "+ExaPrefix+"/search", f.handle(exa, "exa.search", f.exaSearch))
	f.mux.HandleFunc("POST "+ParallelPrefix+"/search", f.handle(parallel, "parallel.search", f.parallelSearch))
}

func (f *Fixture) mistralOCR(x *request) {
	var req struct {
		Model    string
		Document struct {
			Type string
		}
	}
	if !x.decode(&req) || req.Model != MistralModel || req.Document.Type == "" {
		x.fail(http.StatusNotFound, "model_not_found", "The fixture OCR model or document is missing.")
		return
	}
	x.note("ocr", false)
	writeJSON(x.w, http.StatusOK, map[string]any{
		"model": req.Model,
		"pages": []any{map[string]any{
			"index": 0, "markdown": "# Fixture document\n\nFixture OCR text.", "images": []any{},
			"dimensions": map[string]any{"dpi": 200, "height": 2200, "width": 1700},
		}},
		"document_annotation": nil,
		"usage_info":          map[string]any{"pages_processed": 1, "doc_size_bytes": 1048576},
	})
}

func (f *Fixture) perplexitySearch(x *request) {
	var req struct {
		Query string
	}
	if !x.decode(&req) || req.Query == "" {
		x.fail(http.StatusUnprocessableEntity, "missing", "query is required")
		return
	}
	x.note("search", false)
	writeJSON(x.w, http.StatusOK, map[string]any{
		"id": f.nextID("ppx"),
		"results": []any{map[string]any{
			"title": "Fixture result", "url": "https://fixture.example/result",
			"snippet": "Fixture snippet.", "date": "2026-10-01", "last_updated": "2026-10-15",
		}},
		"server_time": "2026-10-19T07:00:00Z",
	})
}

func (f *Fixture) tavilySearch(x *request) {
	var req struct {
		Query string
	}
	if !x.decode(&req) || req.Query == "" {
		x.fail(http.StatusUnprocessableEntity, "missing", "query is required")
		return
	}
	x.note("search", false)
	writeJSON(x.w, http.StatusOK, map[string]any{
		"query": req.Query,
		"results": []any{map[string]any{
			"title": "Fixture result", "url": "https://fixture.example/result",
			"content": "Fixture content.", "score": 0.91,
		}},
		"response_time": 1.2, "request_id": f.nextID("tvly"),
	})
}

func (f *Fixture) exaSearch(x *request) {
	var req struct {
		Query string
	}
	if !x.decode(&req) || req.Query == "" {
		x.fail(http.StatusBadRequest, "invalid_query", "query is required")
		return
	}
	x.note("search", false)
	writeJSON(x.w, http.StatusOK, map[string]any{
		"requestId": f.nextID("exa"),
		"results": []any{map[string]any{
			"id": "doc_fx_1", "title": "Fixture result", "url": "https://fixture.example/result",
			"publishedDate": "2026-10-01T00:00:00Z", "author": "Fixture",
			"highlights": []any{"Fixture highlight one.", "Fixture highlight two."},
		}},
		"costDollars": 0.0125,
	})
}

func (f *Fixture) parallelSearch(x *request) {
	var req struct {
		SearchQueries []string `json:"search_queries"`
	}
	if !x.decode(&req) || len(req.SearchQueries) == 0 {
		x.fail(http.StatusUnprocessableEntity, "missing", "search_queries is required")
		return
	}
	x.note("search", false)
	writeJSON(x.w, http.StatusOK, map[string]any{
		"search_id": f.nextID("par"),
		"results": []any{map[string]any{
			"url": "https://fixture.example/result", "title": "Fixture result",
			"publish_date": "2026-10-01", "excerpts": []any{"Fixture excerpt."},
		}},
		"session_id": f.nextID("par_sess"),
	})
}
