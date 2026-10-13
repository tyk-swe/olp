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
