import {
  optionalDecimal,
  tierRows,
  toolFeeRows,
  windowsDraft
} from './pricingValidation';

// The row shapes the shared price-fields editor binds, and helpers mapping
// them to the price-entry fields pricing revisions and rate cards share.
export type ToolFeeRow = { name: string; fee: string };
export type TierRow = { threshold: string; rates: Record<string, string> };
export type WindowPeriodRow = {
  days: number[];
  start: string;
  end: string;
  multiplier: string;
};

export type PriceFieldValues = {
  inputPrice: string;
  cachedInputPrice: string;
  cacheWritePrice: string;
  cacheWrite5mPrice: string;
  cacheWrite1hPrice: string;
  outputPrice: string;
  unitPrice: string;
  audioInputPrice: string;
  cachedAudioInputPrice: string;
  audioOutputPrice: string;
  imageInputPrice: string;
  cachedImageInputPrice: string;
  imageOutputPrice: string;
  perRequestPrice: string;
  batchMultiplier: string;
  toolFees: ToolFeeRow[];
  tiers: TierRow[];
  windowZone: string;
  windowPeriods: WindowPeriodRow[];
};

export function emptyPriceFields(): PriceFieldValues {
  return {
    inputPrice: '',
    cachedInputPrice: '',
    cacheWritePrice: '',
    cacheWrite5mPrice: '',
    cacheWrite1hPrice: '',
    outputPrice: '',
    unitPrice: '',
    audioInputPrice: '',
    cachedAudioInputPrice: '',
    audioOutputPrice: '',
    imageInputPrice: '',
    cachedImageInputPrice: '',
    imageOutputPrice: '',
    perRequestPrice: '',
    batchMultiplier: '',
    toolFees: [],
    tiers: [],
    windowZone: '',
    windowPeriods: []
  };
}

// The per-million rate fields an entry names, in declaration order; tier
// rows name exactly these.
export function rateFieldPairs(values: {
  inputPrice: string;
  cachedInputPrice: string;
  cacheWritePrice: string;
  cacheWrite5mPrice: string;
  cacheWrite1hPrice: string;
  outputPrice: string;
  audioInputPrice: string;
  cachedAudioInputPrice: string;
  audioOutputPrice: string;
  imageInputPrice: string;
  cachedImageInputPrice: string;
  imageOutputPrice: string;
}): readonly (readonly [string, string])[] {
  return [
    ['input_per_million', values.inputPrice],
    ['cached_input_per_million', values.cachedInputPrice],
    ['cache_write_input_per_million', values.cacheWritePrice],
    ['cache_write_5m_input_per_million', values.cacheWrite5mPrice],
    ['cache_write_1h_input_per_million', values.cacheWrite1hPrice],
    ['output_per_million', values.outputPrice],
    ['audio_input_per_million', values.audioInputPrice],
    ['cached_audio_input_per_million', values.cachedAudioInputPrice],
    ['audio_output_per_million', values.audioOutputPrice],
    ['image_input_per_million', values.imageInputPrice],
    ['cached_image_input_per_million', values.cachedImageInputPrice],
    ['image_output_per_million', values.imageOutputPrice]
  ] as const;
}

export function priceEntryFields(values: PriceFieldValues) {
  return {
    input_per_million: optionalDecimal(values.inputPrice),
    cached_input_per_million: optionalDecimal(values.cachedInputPrice),
    cache_write_input_per_million: optionalDecimal(values.cacheWritePrice),
    cache_write_5m_input_per_million: optionalDecimal(values.cacheWrite5mPrice),
    cache_write_1h_input_per_million: optionalDecimal(values.cacheWrite1hPrice),
    output_per_million: optionalDecimal(values.outputPrice),
    unit_price: optionalDecimal(values.unitPrice),
    audio_input_per_million: optionalDecimal(values.audioInputPrice),
    cached_audio_input_per_million: optionalDecimal(
      values.cachedAudioInputPrice
    ),
    audio_output_per_million: optionalDecimal(values.audioOutputPrice),
    image_input_per_million: optionalDecimal(values.imageInputPrice),
    cached_image_input_per_million: optionalDecimal(
      values.cachedImageInputPrice
    ),
    image_output_per_million: optionalDecimal(values.imageOutputPrice),
    per_request: optionalDecimal(values.perRequestPrice),
    batch_multiplier: optionalDecimal(values.batchMultiplier),
    tool_calls: toolFeeRows(values.toolFees),
    tiers: tierRows(
      values.tiers,
      rateFieldPairs(values)
        .filter(([, value]) => optionalDecimal(value) != null)
        .map(([key]) => key)
    ),
    windows: windowsDraft(values.windowZone, values.windowPeriods)
  };
}
