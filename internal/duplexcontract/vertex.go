package duplexcontract

import (
	"encoding/json"
	"strings"

	"github.com/tyk-swe/olp/internal/geminilifecycle"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/wire"
)

func inspectVertexSetup(frame []byte, maxBytes int) (Setup, error) {
	doc, err := parseObject(frame, maxBytes)
	if err != nil {
		return Setup{}, err
	}
	root := doc.Root()
	if len(root.Members()) != 1 {
		return Setup{}, refuse("invalid_duplex_setup", "/setup", "one_setup_object", "Vertex Live must begin with one setup object.")
	}
	setup, ok := root.Lookup("setup")
	if !ok || setup.Kind() != oif.Object {
		return Setup{}, refuse("invalid_duplex_setup", "/setup", "setup_object", "Vertex Live must begin with setup.")
	}
	model, ok := textField(setup, "model")
	if !ok || model == "" || len(model) > 512 {
		return Setup{}, refuse("invalid_duplex_setup", "/setup/model", "route_model", "Vertex Live setup must name its route model.")
	}
	slug := strings.TrimPrefix(model, "models/")
	if idx := strings.LastIndex(slug, "/models/"); idx >= 0 {
		slug = slug[idx+len("/models/"):]
	}
	if slug == "" || strings.Contains(slug, "/") {
		return Setup{}, refuse("invalid_duplex_setup", "/setup/model", "route_model", "Vertex Live setup must name its route model.")
	}
	for _, aliases := range [][]string{{"generationConfig", "generation_config"}, {"realtimeInputConfig", "realtime_input_config"}, {"sessionResumption", "session_resumption"}, {"inputAudioTranscription", "input_audio_transcription"}} {
		_, a := setup.Lookup(aliases[0])
		_, b := setup.Lookup(aliases[1])
		if a && b {
			return Setup{}, refuse("invalid_duplex_setup", "/setup", "unambiguous_setup", "Vertex Live setup has ambiguous field aliases.")
		}
	}
	for _, name := range []string{"sessionResumption", "session_resumption"} {
		if state, present := setup.Lookup(name); present && state.Kind() == oif.Object {
			if _, hasHandle := state.Lookup("handle"); hasHandle {
				return Setup{}, refuse("invalid_duplex_setup", "/setup/sessionResumption", "connection_scoped_session", "Vertex Live session resumption handles are not admitted on connection-scoped sessions.")
			}
		}
	}
	_, transcription := setup.Lookup("inputAudioTranscription")
	if !transcription {
		_, transcription = setup.Lookup("input_audio_transcription")
	}
	transcriptionOnly := false
	if transcription {
		for _, name := range []string{"generationConfig", "generation_config"} {
			if generation, present := setup.Lookup(name); present && generation.Kind() == oif.Object {
				for _, modalityName := range []string{"responseModalities", "response_modalities"} {
					if modalities, has := generation.Lookup(modalityName); has && modalities.Kind() == oif.Array && len(modalities.Elements()) == 0 {
						transcriptionOnly = true
					}
				}
			}
		}
	}
	return Setup{Model: slug, TranscriptionOnly: transcriptionOnly}, nil
}

func bindVertexSetup(frame []byte, boundModel string, maxBytes int) ([]byte, error) {
	doc, err := parseObject(frame, maxBytes)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(boundModel)
	if err != nil {
		return nil, err
	}
	bound, err := oif.Apply(doc, []oif.Change{{Pointer: "/setup/model", Value: string(encoded), Origin: oif.IdentityBinding, Reason: "qualified Vertex Live serving model"}})
	if err != nil {
		return nil, err
	}
	return bound.Bytes(), nil
}

type vertexObserver struct {
	maxBytes     int
	usage        *wire.Usage
	turn         *wire.Usage
	pending      bool
	awaiting     bool
	submitted    int
	realtimeTurn bool
	incomplete   bool
	setupDone    bool
}

func (o *vertexObserver) serverFrame(frame []byte) (ServerFrame, error) {
	observed, err := geminilifecycle.ObserveLiveServerFrame(frame, o.maxBytes)
	if err != nil {
		return ServerFrame{}, err
	}
	if observed.Kind == "setupComplete" {
		if o.setupDone {
			return ServerFrame{}, refuse("invalid_duplex_frame", "/frame", "one_setup_completion", "Vertex Live repeated its setup completion.")
		}
		o.setupDone = true
	}
	var turnUsage *wire.Usage
	if observed.Usage != nil {
		turnUsage = vertexUsage(observed.Usage)
	}
	if o.awaiting {
		o.awaiting = false
		if observed.Kind == "" && turnUsage != nil {
			o.turn = turnUsage
			o.flushTurn()
			return ServerFrame{}, nil
		}
		o.incomplete = true
	}
	if observed.Kind == "serverContent" || observed.Kind == "toolCall" {
		o.pending = !observed.TurnComplete
	}
	if turnUsage != nil {
		o.turn = turnUsage
	}
	if observed.TurnComplete {
		if o.turn != nil {
			o.flushTurn()
		} else {
			o.awaiting = true
		}
	}
	return ServerFrame{ProviderError: observed.Kind == "error", SetupComplete: observed.Kind == "setupComplete"}, nil
}

func (o *vertexObserver) flushTurn() {
	o.realtimeTurn = false
	if o.submitted > 0 {
		o.submitted--
	}
	if o.usage == nil {
		o.usage = o.turn
		o.turn = nil
		return
	}
	o.usage.InputTokens = addTokens(o.usage.InputTokens, o.turn.InputTokens)
	o.usage.OutputTokens = addTokens(o.usage.OutputTokens, o.turn.OutputTokens)
	o.usage.TotalTokens = addTokens(o.usage.TotalTokens, o.turn.TotalTokens)
	mergeShare(&o.usage.CachedInputTokens, o.turn.CachedInputTokens)
	mergeShare(&o.usage.ReasoningTokens, o.turn.ReasoningTokens)
	mergeShare(&o.usage.AudioInputTokens, o.turn.AudioInputTokens)
	mergeShare(&o.usage.CachedAudioInputTokens, o.turn.CachedAudioInputTokens)
	mergeShare(&o.usage.AudioOutputTokens, o.turn.AudioOutputTokens)
	mergeShare(&o.usage.ImageInputTokens, o.turn.ImageInputTokens)
	mergeShare(&o.usage.CachedImageInputTokens, o.turn.CachedImageInputTokens)
	mergeShare(&o.usage.ImageOutputTokens, o.turn.ImageOutputTokens)
	o.turn = nil
}

func mergeShare(slot **int64, value *int64) {
	if value == nil {
		return
	}
	if *slot == nil {
		v := *value
		*slot = &v
		return
	}
	**slot = addTokens(**slot, *value)
}

func vertexUsage(u *geminilifecycle.LiveUsage) *wire.Usage {
	input, output := *u.Input, *u.Output
	if u.ToolUse != nil {
		input = addTokens(input, *u.ToolUse)
	}
	if u.Thoughts != nil {
		output = addTokens(output, *u.Thoughts)
	}
	usage := &wire.Usage{InputTokens: input, OutputTokens: output, TotalTokens: addTokens(input, output)}
	if u.Total != nil && *u.Total > usage.TotalTokens {
		usage.TotalTokens = *u.Total
	}
	usage.CachedInputTokens = u.Cached
	usage.ReasoningTokens = u.Thoughts
	audioIn, imageIn := modalityShares(u.PromptDetails)
	cachedAudio, cachedImage := modalityShares(u.CacheDetails)
	audioOut, imageOut := modalityShares(u.ResponseDetails)
	usage.AudioInputTokens, usage.ImageInputTokens = audioIn, imageIn
	usage.CachedAudioInputTokens, usage.CachedImageInputTokens = cachedAudio, cachedImage
	usage.AudioOutputTokens, usage.ImageOutputTokens = audioOut, imageOut
	return usage
}

func modalityShares(details []geminilifecycle.LiveUsageDetail) (audio, image *int64) {
	for _, detail := range details {
		switch detail.Modality {
		case "AUDIO":
			v := detail.TokenCount
			audio = &v
		case "IMAGE":
			v := detail.TokenCount
			image = &v
		}
	}
	return audio, image
}

func (o *vertexObserver) clientFrame(frame []byte) error {
	if err := geminilifecycle.ValidateLiveClientFrame(frame, o.maxBytes); err != nil {
		return err
	}
	doc, err := parseObject(frame, o.maxBytes)
	if err != nil {
		return err
	}
	content, contentOK := doc.Root().Lookup("clientContent")
	if !contentOK {
		content, contentOK = doc.Root().Lookup("client_content")
	}
	if contentOK && content.Kind() == oif.Object {
		for _, name := range []string{"turnComplete", "turn_complete"} {
			if complete, present := content.Lookup(name); present && complete.Kind() == oif.Boolean && complete.Raw() == "true" {
				o.submitted++
			}
		}
	}
	input, inputOK := doc.Root().Lookup("realtimeInput")
	if !inputOK {
		input, inputOK = doc.Root().Lookup("realtime_input")
	}
	if inputOK && input.Kind() == oif.Object {
		for _, member := range input.Members() {
			switch member.Name {
			case "activityEnd", "activity_end", "audioStreamEnd", "audio_stream_end", "text":
				o.realtimeTurn = true
			}
		}
	}
	return nil
}

func (o *vertexObserver) usageValue() *wire.Usage { return o.usage }
func (o *vertexObserver) complete() bool {
	return o.usage != nil && o.turn == nil && !o.pending && !o.awaiting && o.submitted == 0 && !o.realtimeTurn && !o.incomplete
}
