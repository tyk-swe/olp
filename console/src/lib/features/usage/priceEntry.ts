import {
  optionalDecimal,
  tierRows,
  toolFeeRows,
  windowsDraft
} from './pricingValidation';

// The row shapes the shared price-fields editor binds, and helpers mapping
// them to the price-entry fields pricing revisions and rate cards share.
export type ToolFeeRow = { name: string; fee: string };
export type ContainerFeeRow = { tier: string; fee: string };
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
  trainingPrice: string;
  storagePrice: string;
  cacheStoragePrice: string;
  containerFees: ContainerFeeRow[];
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
    trainingPrice: '',
    storagePrice: '',
    cacheStoragePrice: '',
    containerFees: [],
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
    training_per_million: optionalDecimal(values.trainingPrice),
    storage_per_gib_day: optionalDecimal(values.storagePrice),
    cache_storage_per_million_token_hour: optionalDecimal(
      values.cacheStoragePrice
    ),
    container_per_session: containerFeeRows(values.containerFees),
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

// Memory tiers are native billing keys. Keep decimal fees as source strings;
// converting them to numbers would lose precision before server settlement.
export function containerFeeRows(
  rows: ContainerFeeRow[]
): Record<string, string> | null {
  const fees: Record<string, string> = {};
  for (const row of rows) {
    const tier = row.tier.trim();
    if (!tier && !row.fee.trim()) continue;
    if (!/^[1-9][0-9]{0,4}g$/.test(tier))
      throw new Error('Enter a container memory tier such as 1g, 4g or 16g.');
    if (Object.hasOwn(fees, tier))
      throw new Error(`Container tier ${tier} is listed twice.`);
    const fee = optionalDecimal(row.fee);
    if (fee === null)
      throw new Error(`Enter a session fee for container tier ${tier}.`);
    fees[tier] = fee;
  }
  if (Object.keys(fees).length > 16)
    throw new Error('At most 16 container memory tiers per price.');
  return Object.keys(fees).length ? fees : null;
}
