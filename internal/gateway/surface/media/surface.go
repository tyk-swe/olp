package media

import (
	"net/http"
	"time"

	"github.com/tyk-swe/olp/internal/domain/operations/tokenization/estimate"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/media/jobs"
	"github.com/tyk-swe/olp/internal/media/transport"
	"github.com/tyk-swe/olp/internal/platform/staging"
)

type Deps struct {
	Transport *transport.Transport
	Jobs      *jobs.Service
	Store     *jobs.Store
}

type surface struct {
	kernel *kernel.Server
	deps   *Deps
}

func Register(k *kernel.Server, mux *http.ServeMux, deps *Deps) {
	if deps == nil {
		return
	}
	s := surface{kernel: k, deps: deps}
	mux.HandleFunc("POST /v1/images/generations", s.jsonHandler(media.DecodeImageGeneration))
	mux.HandleFunc("POST /v1/images/edits", s.formHandler(media.DefaultImageUploadLimit, 33, media.DecodeImageEdit))
	mux.HandleFunc("POST /v1/images/variations", s.formHandler(media.DefaultImageUploadLimit, 1, media.DecodeImageVariation))
	mux.HandleFunc("POST /v1/audio/speech", s.jsonHandler(media.DecodeSpeech))
	mux.HandleFunc("POST /v1/audio/transcriptions", s.formHandler(media.DefaultAudioUploadLimit, 1, media.DecodeTranscription))
	mux.HandleFunc("POST /v1/audio/translations", s.formHandler(media.DefaultAudioUploadLimit, 1, media.DecodeTranslation))
	mux.HandleFunc("POST /v1/videos", s.videoCreate)
	mux.HandleFunc("GET /v1/videos", s.videoList)
	mux.HandleFunc("GET /v1/videos/{video_id}", s.videoGet)
	mux.HandleFunc("GET /v1/videos/{video_id}/content", s.videoContent)
	mux.HandleFunc("DELETE /v1/videos/{video_id}", s.videoDelete)
}

func (s surface) jsonHandler(decode func([]byte) (*media.Request, *media.Error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		x, authority, done := s.kernel.EnterPrioritized(w, r, wire.Chat)
		if done {
			return
		}
		body, e := s.readJSONBody(w, r)
		if e != nil {
			s.kernel.Gate.Release(r.Context())
			s.kernel.Refuse(x, w, e)
			return
		}
		request, failure := decode(body)
		if failure != nil {
			s.kernel.Gate.Release(r.Context())
			s.kernel.Refuse(x, w, mediaError(failure))
			return
		}
		defer s.kernel.Gate.Release(r.Context())
		sized := estimate.HeuristicBytesTokens(len(body))
		s.serveMedia(r.Context(), w, x, authority, request, &sized)
	}
}

func (s surface) readJSONBody(w http.ResponseWriter, r *http.Request) ([]byte, *fault.Error) {
	rc := http.NewResponseController(w)
	if err := rc.SetReadDeadline(time.Now().Add(kernel.RequestBodyTimeout)); err != nil {
		return nil, fault.ServerError(http.StatusInternalServerError, "internal_error", "The request could not be read.")
	}
	body, e := s.kernel.Gate.ReadBody(r)
	if e != nil {
		return nil, e
	}
	_ = rc.SetReadDeadline(time.Time{})
	return body, nil
}

func (s surface) formHandler(maxFileBytes int64, maxFiles int, decode func(*staging.Form) (*media.Request, *media.Error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		x, authority, done := s.kernel.EnterPrioritized(w, r, wire.Chat)
		if done {
			return
		}
		form, e := s.kernel.ParseMultipartForm(w, r, authority.ID, maxFileBytes, maxFiles)
		if e != nil {
			s.kernel.Gate.Release(r.Context())
			s.kernel.Refuse(x, w, e)
			return
		}
		request, failure := decode(form)
		if failure != nil {
			form.Cleanup()
			s.kernel.Gate.Release(r.Context())
			s.kernel.Refuse(x, w, mediaError(failure))
			return
		}
		defer s.kernel.Gate.Release(r.Context())
		form.Disarm()
		s.serveMedia(r.Context(), w, x, authority, request, nil)
	}
}

func (s surface) videoCreate(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.kernel.EnterPrioritized(w, r, wire.Chat)
	if done {
		return
	}
	form, request, e := s.readVideoCreate(w, r, authority)
	if e != nil {
		s.kernel.Gate.Release(r.Context())
		s.kernel.Refuse(x, w, e)
		return
	}
	defer s.kernel.Gate.Release(r.Context())
	s.createVideoJob(w, r, x, authority, form, request)
}
