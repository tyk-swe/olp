//go:build integration

package usage

import (
	"encoding/json"
	"testing"
	"time"
)

// TestIntegrationAttemptUsageFactDimensions persists an attempt that uses every
// pricing dimension and reads back the fact, its charged components and the
// hourly rollup the counters fold into.
func TestIntegrationAttemptUsageFactDimensions(t *testing.T) {
	pool := notificationPool(t)
	owner, key := seedParityOwner(t, pool)
	provider := seedParityProvider(t, pool, owner)
	observed := time.Now().UTC().Add(-time.Minute)
	price := Price{ProviderKind: "openai", Model: "parity-dims", Operation: "generation",
		InputPerMillion: rate("1"), CachedInputPerMillion: rate("0.5"), OutputPerMillion: rate("2"),
		AudioInputPerMillion: rate("4"), CachedAudioInputPerMillion: rate("0.25"),
		AudioOutputPerMillion: rate("8"), ImageInputPerMillion: rate("6"), ImageOutputPerMillion: rate("12"),
		PerRequest: rate("0.1"), BatchMultiplier: rate("0.5"),
		ToolCalls: map[string]string{"web_search": "0.01"},
		Windows: &PriceWindows{TimeZone: "UTC", Periods: []PriceWindowPeriod{{
			Days: []int{1, 2, 3, 4, 5, 6, 7}, Start: "00:00", End: "24:00", Multiplier: "2"}}},
		Tiers: []PriceTier{{AboveInputTokens: 1, InputPerMillion: rate("3"),
			CachedInputPerMillion: rate("1.5"), OutputPerMillion: rate("4"),
			AudioInputPerMillion: rate("8"), CachedAudioInputPerMillion: rate("0.5"),
			AudioOutputPerMillion: rate("16"), ImageInputPerMillion: rate("12"), ImageOutputPerMillion: rate("24")}},
		Currency: "USD"}
	seedParityPrices(t, pool, owner, observed.Add(-time.Hour), []parityCase{{model: price.Model, price: price}})

	usage := AttemptUsage{Observed: true, Complete: true,
		InputTokens: tokens(1000), OutputTokens: tokens(100),
		CachedInputTokens: tokens(200), CachedAudioInputTokens: tokens(100),
		AudioInputTokens: tokens(400), ImageInputTokens: tokens(100),
		AudioOutputTokens: tokens(20), ImageOutputTokens: tokens(10),
		ToolCalls: map[string]int64{"web_search": 2}, Batch: true}
	event := parityEvent(t, key, provider, observed, []parityCase{{model: price.Model, usage: usage}})
	result, _ := persistParity(t, pool, event)
	if result.Outcome != PersistOutcomePersisted {
		t.Fatalf("outcome = %v", result.Outcome)
	}

	// The fact stores the dimensions it billed and the components it charged.
	var audio, cachedAudio, audioOut, image, cachedImage, imageOut *int64
	var toolCalls []byte
	var batch bool
	var components []string
	var cost *string
	if err := pool.QueryRow(t.Context(), `SELECT audio_input_tokens, cached_audio_input_tokens,
	    audio_output_tokens, image_input_tokens, cached_image_input_tokens, image_output_tokens,
	    tool_calls::text, batch, pricing_components, estimated_cost::text
	    FROM olp.attempt_usage_facts WHERE event_id=$1::uuid`, event.EventID).
		Scan(&audio, &cachedAudio, &audioOut, &image, &cachedImage, &imageOut,
			&toolCalls, &batch, &components, &cost); err != nil {
		t.Fatalf("read fact: %v", err)
	}
	if *audio != 400 || *cachedAudio != 100 || *audioOut != 20 ||
		*image != 100 || cachedImage != nil || *imageOut != 10 {
		t.Fatalf("modality columns = %v %v %v %v %v %v",
			valOrZero(audio), valOrZero(cachedAudio), valOrZero(audioOut),
			valOrZero(image), valOrZero(cachedImage), valOrZero(imageOut))
	}
	if !batch {
		t.Fatal("batch was not recorded")
	}
	var tools map[string]int64
	if err := json.Unmarshal(toolCalls, &tools); err != nil || tools["web_search"] != 2 {
		t.Fatalf("tool_calls = %s %v", toolCalls, err)
	}
	// Input exceeds the tier threshold so tier rates apply: text 400 @3,
	// cached-text 100 @1.5, cached-audio 100 @0.5, audio 300 @8, image 100 @12
	// is 1200+150+50+2400+1200 = 5000/1e6; output text 70 @4, audio 20 @16,
	// image 10 @24 is 280+320+240 = 840/1e6; plus the request fee and two tool
	// calls; the batch half and the window's double cancel out.
	want := "0.125840000000"
	if cost == nil || *cost != want {
		t.Fatalf("estimated_cost = %v, want %s", cost, want)
	}
	for _, component := range []string{"tiers", "windows", "batch_multiplier", "per_request",
		"input_per_million", "cached_input_per_million", "cached_audio_input_per_million",
		"audio_input_per_million", "image_input_per_million",
		"output_per_million", "audio_output_per_million", "image_output_per_million",
		"tool_calls.web_search"} {
		found := false
		for _, got := range components {
			found = found || got == component
		}
		if !found {
			t.Fatalf("components = %v, missing %s", components, component)
		}
	}

	// Request history returns the attempt's dimensions and components.
	detail, err := GetRequest(t.Context(), pool, event.RequestID)
	if err != nil {
		t.Fatalf("read detail: %v", err)
	}
	if len(detail.Attempts) != 1 {
		t.Fatalf("attempts = %d", len(detail.Attempts))
	}
	attempt := detail.Attempts[0]
	if attempt.AudioInputTokens == nil || *attempt.AudioInputTokens != 400 ||
		attempt.Batch == nil || !*attempt.Batch || len(attempt.PricingComponents) == 0 ||
		attempt.ToolCalls["web_search"] != 2 {
		t.Fatalf("history attempt = %+v", attempt)
	}

	// The dimensions survive the hourly rollup.
	conn, err := pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	_, facts, err := rollUpAttemptUsage(t.Context(), conn.Conn(), observed.Add(2*time.Hour))
	conn.Release()
	if err != nil || facts != 1 {
		t.Fatalf("rollup folded %d facts: %v", facts, err)
	}
	var hourlyAudio, hourlyCachedAudio, hourlyAudioOut, hourlyImage, hourlyCachedImage, hourlyImageOut string
	if err := pool.QueryRow(t.Context(), `SELECT audio_input_tokens::text, cached_audio_input_tokens::text,
	    audio_output_tokens::text, image_input_tokens::text, cached_image_input_tokens::text,
	    image_output_tokens::text FROM olp.attempt_usage_hourly WHERE route_slug='parity'`).
		Scan(&hourlyAudio, &hourlyCachedAudio, &hourlyAudioOut, &hourlyImage, &hourlyCachedImage, &hourlyImageOut); err != nil {
		t.Fatalf("read hourly: %v", err)
	}
	if hourlyAudio != "400" || hourlyCachedAudio != "100" || hourlyAudioOut != "20" ||
		hourlyImage != "100" || hourlyCachedImage != "0" || hourlyImageOut != "10" {
		t.Fatalf("hourly modality sums = %s %s %s %s %s %s",
			hourlyAudio, hourlyCachedAudio, hourlyAudioOut, hourlyImage, hourlyCachedImage, hourlyImageOut)
	}

}
