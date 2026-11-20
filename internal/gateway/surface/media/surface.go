package media

import (
	"net/http"
	"time"

	"github.com/tyk-swe/olp/internal/domain/operations/tokenization/estimate"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/staging"
)

type surface struct{ kernel *kernel.Server }

func Register(k *kernel.Server, mux *http.ServeMux) {
	if k.Deps.Media == nil {
		return
	}
	s := surface{kernel: k}
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
		x, authority, done := s.kernel.BeginMedia(w, r)
		if done {
			return
		}
		rc := http.NewResponseController(w)
		if err := rc.SetReadDeadline(time.Now().Add(kernel.RequestBodyTimeout)); err != nil {
			s.kernel.Gate.Release(r.Context())
			s.kernel.Refuse(x, w, fault.ServerError(http.StatusInternalServerError, "internal_error", "The request could not be read."))
			return
		}
		body, e := s.kernel.Gate.ReadBody(r)
		if e != nil {
			s.kernel.Gate.Release(r.Context())
			s.kernel.Refuse(x, w, e)
			return
		}
		rc.SetReadDeadline(time.Time{})
		request, failure := decode(body)
		if failure != nil {
			s.kernel.Gate.Release(r.Context())
			s.kernel.Refuse(x, w, kernel.MediaError(failure))
			return
		}
		sized := estimate.HeuristicBytesTokens(len(body))
		x.NoteDecodedMedia(request, &sized)
		s.kernel.ServeMedia(r.Context(), w, x, authority, request)
	}
}

func (s surface) formHandler(maxFileBytes int64, maxFiles int, decode func(*staging.Form) (*media.Request, *media.Error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		x, authority, done := s.kernel.BeginMedia(w, r)
		if done {
			return
		}
		form, e := s.kernel.ParseMediaForm(w, r, authority.ID, maxFileBytes, maxFiles)
		if e != nil {
			s.kernel.Gate.Release(r.Context())
			s.kernel.Refuse(x, w, e)
			return
		}
		request, failure := decode(form)
		if failure != nil {
			form.Cleanup()
			s.kernel.Gate.Release(r.Context())
			s.kernel.Refuse(x, w, kernel.MediaError(failure))
			return
		}
		form.Disarm()
		x.NoteDecodedMedia(request, nil)
		s.kernel.ServeMedia(r.Context(), w, x, authority, request)
	}
}

func (s surface) videoCreate(w http.ResponseWriter, r *http.Request) {
	x, authority, done := s.kernel.BeginMedia(w, r)
	if done {
		return
	}
	form, request, e := s.kernel.ReadVideoCreate(w, r, authority)
	if e != nil {
		s.kernel.Gate.Release(r.Context())
		s.kernel.Refuse(x, w, e)
		return
	}
	defer s.kernel.Gate.Release(r.Context())
	s.kernel.CreateVideoJob(w, r, x, authority, form, request)
}
