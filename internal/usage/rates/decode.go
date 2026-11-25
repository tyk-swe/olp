package rates

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

type jsonDecimal string

func (d *jsonDecimal) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return err
		}
		*d = jsonDecimal(s)
		return nil
	}
	var num json.Number
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	if err := dec.Decode(&num); err != nil {
		return err
	}
	*d = jsonDecimal(num.String())
	return nil
}

func (d *jsonDecimal) ptr() *string {
	if d == nil {
		return nil
	}
	s := string(*d)
	return &s
}

func decodeDecimalMap(src map[string]jsonDecimal) map[string]string {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]string, len(src))
	for k, v := range src {
		dst[k] = string(v)
	}
	return dst
}

type tierRowJSON struct {
	AboveInputTokens            int64        `json:"above_input_tokens"`
	InputPerMillion             *jsonDecimal `json:"input_per_million"`
	CachedInputPerMillion       *jsonDecimal `json:"cached_input_per_million"`
	CacheWriteInputPerMillion   *jsonDecimal `json:"cache_write_input_per_million"`
	CacheWrite5MInputPerMillion *jsonDecimal `json:"cache_write_5m_input_per_million"`
	CacheWrite1HInputPerMillion *jsonDecimal `json:"cache_write_1h_input_per_million"`
	OutputPerMillion            *jsonDecimal `json:"output_per_million"`
	AudioInputPerMillion        *jsonDecimal `json:"audio_input_per_million"`
	CachedAudioInputPerMillion  *jsonDecimal `json:"cached_audio_input_per_million"`
	AudioOutputPerMillion       *jsonDecimal `json:"audio_output_per_million"`
	ImageInputPerMillion        *jsonDecimal `json:"image_input_per_million"`
	CachedImageInputPerMillion  *jsonDecimal `json:"cached_image_input_per_million"`
	ImageOutputPerMillion       *jsonDecimal `json:"image_output_per_million"`
}

type windowPeriodRowJSON struct {
	Days       []int       `json:"days"`
	Start      string      `json:"start"`
	End        string      `json:"end"`
	Multiplier jsonDecimal `json:"multiplier"`
}

type windowsRowJSON struct {
	TimeZone string                `json:"time_zone"`
	Periods  []windowPeriodRowJSON `json:"periods"`
}

type priceRowJSON struct {
	ProviderKind                    string                 `json:"provider_kind"`
	VendorID                        *string                `json:"vendor_id"`
	ProviderID                      *string                `json:"provider_id"`
	Model                           string                 `json:"model"`
	Operation                       string                 `json:"operation"`
	Currency                        string                 `json:"currency"`
	InputPerMillion                 *jsonDecimal           `json:"input_per_million"`
	CachedInputPerMillion           *jsonDecimal           `json:"cached_input_per_million"`
	CacheWriteInputPerMillion       *jsonDecimal           `json:"cache_write_input_per_million"`
	CacheWrite5MInputPerMillion     *jsonDecimal           `json:"cache_write_5m_input_per_million"`
	CacheWrite1HInputPerMillion     *jsonDecimal           `json:"cache_write_1h_input_per_million"`
	OutputPerMillion                *jsonDecimal           `json:"output_per_million"`
	AudioInputPerMillion            *jsonDecimal           `json:"audio_input_per_million"`
	CachedAudioInputPerMillion      *jsonDecimal           `json:"cached_audio_input_per_million"`
	AudioOutputPerMillion           *jsonDecimal           `json:"audio_output_per_million"`
	ImageInputPerMillion            *jsonDecimal           `json:"image_input_per_million"`
	CachedImageInputPerMillion      *jsonDecimal           `json:"cached_image_input_per_million"`
	ImageOutputPerMillion           *jsonDecimal           `json:"image_output_per_million"`
	UnitPrice                       *jsonDecimal           `json:"unit_price"`
	PerRequest                      *jsonDecimal           `json:"per_request"`
	BatchMultiplier                 *jsonDecimal           `json:"batch_multiplier"`
	TrainingPerMillion              *jsonDecimal           `json:"training_per_million"`
	StoragePerGiBDay                *jsonDecimal           `json:"storage_per_gib_day"`
	CacheStoragePerMillionTokenHour *jsonDecimal           `json:"cache_storage_per_million_token_hour"`
	ToolCalls                       map[string]jsonDecimal `json:"tool_calls"`
	ContainerPerSession             map[string]jsonDecimal `json:"container_per_session"`
	Tiers                           []tierRowJSON          `json:"tiers"`
	Windows                         *windowsRowJSON        `json:"windows"`
}

// DecodePriceRowJSON decodes a PostgreSQL to_jsonb(olp.prices) or
// to_jsonb(olp.rate_card_prices) document into a Price without float64 loss.
func DecodePriceRowJSON(raw []byte) (Price, error) {
	var row priceRowJSON
	if err := json.Unmarshal(raw, &row); err != nil {
		return Price{}, fmt.Errorf("decode price row: %w", err)
	}
	return Price{
		ProviderKind:                    row.ProviderKind,
		VendorID:                        row.VendorID,
		ProviderID:                      row.ProviderID,
		Model:                           row.Model,
		Operation:                       row.Operation,
		Currency:                        strings.TrimSpace(row.Currency),
		InputPerMillion:                 row.InputPerMillion.ptr(),
		CachedInputPerMillion:           row.CachedInputPerMillion.ptr(),
		CacheWriteInputPerMillion:       row.CacheWriteInputPerMillion.ptr(),
		CacheWrite5MInputPerMillion:     row.CacheWrite5MInputPerMillion.ptr(),
		CacheWrite1HInputPerMillion:     row.CacheWrite1HInputPerMillion.ptr(),
		OutputPerMillion:                row.OutputPerMillion.ptr(),
		AudioInputPerMillion:            row.AudioInputPerMillion.ptr(),
		CachedAudioInputPerMillion:      row.CachedAudioInputPerMillion.ptr(),
		AudioOutputPerMillion:           row.AudioOutputPerMillion.ptr(),
		ImageInputPerMillion:            row.ImageInputPerMillion.ptr(),
		CachedImageInputPerMillion:      row.CachedImageInputPerMillion.ptr(),
		ImageOutputPerMillion:           row.ImageOutputPerMillion.ptr(),
		UnitPrice:                       row.UnitPrice.ptr(),
		PerRequest:                      row.PerRequest.ptr(),
		BatchMultiplier:                 row.BatchMultiplier.ptr(),
		TrainingPerMillion:              row.TrainingPerMillion.ptr(),
		StoragePerGiBDay:                row.StoragePerGiBDay.ptr(),
		CacheStoragePerMillionTokenHour: row.CacheStoragePerMillionTokenHour.ptr(),
		ToolCalls:                       decodeDecimalMap(row.ToolCalls),
		ContainerPerSession:             decodeDecimalMap(row.ContainerPerSession),
		Tiers:                           decodeTiersJSON(row.Tiers),
		Windows:                         decodeWindowsJSON(row.Windows),
	}, nil
}

func decodeTiersJSON(rows []tierRowJSON) []PriceTier {
	if len(rows) == 0 {
		return nil
	}
	out := make([]PriceTier, len(rows))
	for i, tier := range rows {
		out[i] = PriceTier{
			AboveInputTokens:            tier.AboveInputTokens,
			InputPerMillion:             tier.InputPerMillion.ptr(),
			CachedInputPerMillion:       tier.CachedInputPerMillion.ptr(),
			CacheWriteInputPerMillion:   tier.CacheWriteInputPerMillion.ptr(),
			CacheWrite5MInputPerMillion: tier.CacheWrite5MInputPerMillion.ptr(),
			CacheWrite1HInputPerMillion: tier.CacheWrite1HInputPerMillion.ptr(),
			OutputPerMillion:            tier.OutputPerMillion.ptr(),
			AudioInputPerMillion:        tier.AudioInputPerMillion.ptr(),
			CachedAudioInputPerMillion:  tier.CachedAudioInputPerMillion.ptr(),
			AudioOutputPerMillion:       tier.AudioOutputPerMillion.ptr(),
			ImageInputPerMillion:        tier.ImageInputPerMillion.ptr(),
			CachedImageInputPerMillion:  tier.CachedImageInputPerMillion.ptr(),
			ImageOutputPerMillion:       tier.ImageOutputPerMillion.ptr(),
		}
	}
	return out
}

func decodeWindowsJSON(row *windowsRowJSON) *PriceWindows {
	if row == nil {
		return nil
	}
	periods := make([]PriceWindowPeriod, len(row.Periods))
	for i, period := range row.Periods {
		periods[i] = PriceWindowPeriod{
			Days:       period.Days,
			Start:      period.Start,
			End:        period.End,
			Multiplier: string(period.Multiplier),
		}
	}
	return &PriceWindows{TimeZone: row.TimeZone, Periods: periods}
}
