package rates

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/tyk-swe/olp/internal/usage"
)

// Price is one rate in a pricing revision. Rates are exact decimal strings:
// stored as numeric. PostgreSQL and the Go admission mirror round money once
// at settlement precision after applying all quantities and multipliers.
type Price struct {
	TrainingPerMillion              *string           `json:"training_per_million"`
	StoragePerGiBDay                *string           `json:"storage_per_gib_day"`
	CacheStoragePerMillionTokenHour *string           `json:"cache_storage_per_million_token_hour"`
	ContainerPerSession             map[string]string `json:"container_per_session"`
	VendorID                        *string           `json:"vendor_id"`
	ProviderKind                    string            `json:"provider_kind"`
	ProviderID                      *string           `json:"provider_id"`
	Model                           string            `json:"model"`
	Operation                       string            `json:"operation"`
	InputPerMillion                 *string           `json:"input_per_million"`
	// CachedInputPerMillion rates the cached share of the input tokens. Absent
	// means cached tokens bill at the full input rate.
	CachedInputPerMillion *string `json:"cached_input_per_million"`

	CacheWriteInputPerMillion   *string `json:"cache_write_input_per_million"`
	CacheWrite5MInputPerMillion *string `json:"cache_write_5m_input_per_million"`
	CacheWrite1HInputPerMillion *string `json:"cache_write_1h_input_per_million"`
	OutputPerMillion            *string `json:"output_per_million"`

	// The modality shares bill at their own per-million rates when the price
	// names them, and fall back to the text rate when it does not.
	AudioInputPerMillion       *string `json:"audio_input_per_million"`
	CachedAudioInputPerMillion *string `json:"cached_audio_input_per_million"`
	AudioOutputPerMillion      *string `json:"audio_output_per_million"`
	ImageInputPerMillion       *string `json:"image_input_per_million"`
	CachedImageInputPerMillion *string `json:"cached_image_input_per_million"`
	ImageOutputPerMillion      *string `json:"image_output_per_million"`

	UnitPrice *string `json:"unit_price"`
	// PerRequest is a fixed fee charged once per successful attempt.
	PerRequest *string `json:"per_request"`
	// BatchMultiplier scales the whole usage charge of usage marked as batch.
	BatchMultiplier *string `json:"batch_multiplier"`
	// ToolCalls is a fee per metered tool invocation, keyed by tool name.
	ToolCalls map[string]string `json:"tool_calls"`
	// Tiers replace the whole per-million rate set once an attempt's input
	// tokens exceed their threshold; Windows scale the charge by time of day
	// in a named zone.
	Tiers   []PriceTier   `json:"tiers"`
	Windows *PriceWindows `json:"windows"`

	Currency string `json:"currency"`
}

// PriceTier is one input-token threshold and the per-million rates that apply
// above it. It names exactly the per-million fields its price names.
type PriceTier struct {
	AboveInputTokens            int64   `json:"above_input_tokens"`
	InputPerMillion             *string `json:"input_per_million"`
	CachedInputPerMillion       *string `json:"cached_input_per_million"`
	CacheWriteInputPerMillion   *string `json:"cache_write_input_per_million"`
	CacheWrite5MInputPerMillion *string `json:"cache_write_5m_input_per_million"`
	CacheWrite1HInputPerMillion *string `json:"cache_write_1h_input_per_million"`
	OutputPerMillion            *string `json:"output_per_million"`
	AudioInputPerMillion        *string `json:"audio_input_per_million"`
	CachedAudioInputPerMillion  *string `json:"cached_audio_input_per_million"`
	AudioOutputPerMillion       *string `json:"audio_output_per_million"`
	ImageInputPerMillion        *string `json:"image_input_per_million"`
	CachedImageInputPerMillion  *string `json:"cached_image_input_per_million"`
	ImageOutputPerMillion       *string `json:"image_output_per_million"`
}

// PriceWindows scales an attempt's whole usage charge by the time of day and
// day of week it started at, evaluated in the named IANA time zone.
type PriceWindows struct {
	TimeZone string              `json:"time_zone"`
	Periods  []PriceWindowPeriod `json:"periods"`
}

// PriceWindowPeriod is one recurring window: the days it applies on (ISO
// weekday numbers, 1 = Monday), the local start inclusive and end exclusive,
// and the multiplier it applies while it holds. An end of "24:00" runs the
// period through midnight.
type PriceWindowPeriod struct {
	Days       []int  `json:"days"`
	Start      string `json:"start"`
	End        string `json:"end"`
	Multiplier string `json:"multiplier"`
}

// Cost prices one attempt's usage against this price, exactly as PostgreSQL
// does when the attempt is accounted: the amount, and false when PostgreSQL
// would record no cost because the usage is incomplete or a dimension it used
// has no rate. startedAt decides which pricing window, if any, the attempt fell
// in, and successful decides whether a per-request fee applies.
func (p Price) Cost(u usage.AttemptUsage, startedAt time.Time, successful bool) (Cost, bool) {
	return parseRates(&p).cost(u, startedAt, successful, nil)
}

func UnmarshalPriceComponents(p *Price, toolCalls, tiers, windows *string, label string) error {
	if toolCalls != nil {
		if err := json.Unmarshal([]byte(*toolCalls), &p.ToolCalls); err != nil {
			return fmt.Errorf("stored %s tool fees are invalid", label)
		}
	}
	if tiers != nil {
		if err := json.Unmarshal([]byte(*tiers), &p.Tiers); err != nil {
			return fmt.Errorf("stored %s tiers are invalid", label)
		}
	}
	if windows != nil {
		p.Windows = &PriceWindows{}
		if err := json.Unmarshal([]byte(*windows), p.Windows); err != nil {
			return fmt.Errorf("stored %s windows are invalid", label)
		}
	}
	return nil
}
