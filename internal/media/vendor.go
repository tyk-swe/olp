package media

import (
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/vendors"
)

type encoder[P Params] func(r *Request, p P, cfg connectors.Config, model string) (*UpstreamCall, *Error)

type codec struct {
	imageGeneration encoder[ImageGeneration]
	imageEdit       encoder[ImageEdit]
	imageVariation  encoder[ImageVariation]
	speech          encoder[Speech]
	transcription   encoder[Transcription]
	translation     encoder[Translation]
	videoCreate     encoder[VideoCreate]
	videoGet        func(VideoGet) (*UpstreamCall, *Error)
	videoContent    func(VideoContent) (*UpstreamCall, *Error)
	videoDelete     func(VideoDelete) (*UpstreamCall, *Error)
}

var openAICodec = codec{
	imageGeneration: encodeImageGeneration,
	imageEdit:       encodeImageEdit,
	imageVariation:  encodeImageVariation,
	speech:          encodeSpeech,
	transcription:   encodeTranscription,
	translation:     encodeTranslation,
	videoCreate:     encodeVideoCreate,
	videoGet:        encodeVideoGet,
	videoContent:    encodeVideoContent,
	videoDelete:     encodeVideoDelete,
}

var codecs = map[string]codec{
	"assemblyai": assemblyAICodec,
	"bfl":        bflCodec,
	"deepgram":   deepgramCodec,
	"elevenlabs": elevenLabsCodec,
	"gemini":     geminiMediaCodec,
	"groq-audio": groqAudioCodec,
	"polly":      pollyCodec,
	"recraft":    recraftCodec,
	"runway":     runwayCodec,
	"stability":  stabilityCodec,
	"xai-images": xaiImagesCodec,
}

func codecFor(cfg connectors.Config, op Operation) (codec, bool) {
	if wire := vendors.MediaWire(cfg.VendorID, string(op)); wire != "" {
		return codecs[wire], true
	}
	return openAICodec, false
}

func (c codec) encode(r *Request, cfg connectors.Config, model string) (*UpstreamCall, *Error) {
	switch p := r.Params().(type) {
	case ImageGeneration:
		return run(c.imageGeneration, r, p, cfg, model)
	case ImageEdit:
		return run(c.imageEdit, r, p, cfg, model)
	case ImageVariation:
		return run(c.imageVariation, r, p, cfg, model)
	case Speech:
		return run(c.speech, r, p, cfg, model)
	case Transcription:
		return run(c.transcription, r, p, cfg, model)
	case Translation:
		return run(c.translation, r, p, cfg, model)
	case VideoCreate:
		return run(c.videoCreate, r, p, cfg, model)
	}
	return nil, unservedOperation()
}

func run[P Params](encode encoder[P], r *Request, p P, cfg connectors.Config, model string) (*UpstreamCall, *Error) {
	if encode == nil {
		return nil, unservedOperation()
	}
	return encode(r, p, cfg, model)
}

func EncodeJob(job JobCall, cfg connectors.Config) (*UpstreamCall, *Error) {
	c, _ := codecFor(cfg, job.Operation())
	switch job := job.(type) {
	case VideoGet:
		return runJob(c.videoGet, job)
	case VideoContent:
		return runJob(c.videoContent, job)
	case VideoDelete:
		return runJob(c.videoDelete, job)
	}
	return nil, unservedOperation()
}

func runJob[J JobCall](encode func(J) (*UpstreamCall, *Error), job J) (*UpstreamCall, *Error) {
	if encode == nil {
		return nil, unservedOperation()
	}
	return encode(job)
}

func unservedOperation() *Error {
	return invalidMedia("The vendor's media codec does not serve this operation.")
}
