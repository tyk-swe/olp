<script lang="ts">
  import {
    rateFieldPairs,
    type TierRow,
    type ToolFeeRow,
    type ContainerFeeRow,
    type WindowPeriodRow
  } from './priceEntry';
  import PriceExtraFields from './PriceExtraFields.svelte';

  // The shared editor for one price entry's rate fields and its additional
  // components: pricing revisions and rate card revisions both carry them.
  let {
    idPrefix,
    canEdit = true,
    inputPrice = $bindable(''),
    cachedInputPrice = $bindable(''),
    cacheWritePrice = $bindable(''),
    cacheWrite5mPrice = $bindable(''),
    cacheWrite1hPrice = $bindable(''),
    outputPrice = $bindable(''),
    unitPrice = $bindable(''),
    audioInputPrice = $bindable(''),
    cachedAudioInputPrice = $bindable(''),
    audioOutputPrice = $bindable(''),
    imageInputPrice = $bindable(''),
    cachedImageInputPrice = $bindable(''),
    imageOutputPrice = $bindable(''),
    perRequestPrice = $bindable(''),
    trainingPrice = $bindable(''),
    storagePrice = $bindable(''),
    cacheStoragePrice = $bindable(''),
    containerFees = $bindable<ContainerFeeRow[]>([]),
    batchMultiplier = $bindable(''),
    toolFees = $bindable<ToolFeeRow[]>([]),
    tiers = $bindable<TierRow[]>([]),
    windowZone = $bindable(''),
    windowPeriods = $bindable<WindowPeriodRow[]>([])
  }: {
    idPrefix: string;
    canEdit?: boolean;
    inputPrice?: string;
    cachedInputPrice?: string;
    cacheWritePrice?: string;
    cacheWrite5mPrice?: string;
    cacheWrite1hPrice?: string;
    outputPrice?: string;
    unitPrice?: string;
    audioInputPrice?: string;
    cachedAudioInputPrice?: string;
    audioOutputPrice?: string;
    imageInputPrice?: string;
    cachedImageInputPrice?: string;
    imageOutputPrice?: string;
    perRequestPrice?: string;
    trainingPrice?: string;
    storagePrice?: string;
    cacheStoragePrice?: string;
    containerFees?: ContainerFeeRow[];
    batchMultiplier?: string;
    toolFees?: ToolFeeRow[];
    tiers?: TierRow[];
    windowZone?: string;
    windowPeriods?: WindowPeriodRow[];
  } = $props();

  // The per-million rate fields the entry carries decide every tier's rate
  // set: a tier names exactly the same fields.
  const rateFields = $derived(
    rateFieldPairs({
      inputPrice,
      cachedInputPrice,
      cacheWritePrice,
      cacheWrite5mPrice,
      cacheWrite1hPrice,
      outputPrice,
      audioInputPrice,
      cachedAudioInputPrice,
      audioOutputPrice,
      imageInputPrice,
      cachedImageInputPrice,
      imageOutputPrice
    })
  );
</script>

<div class="form-field">
  <label for={`${idPrefix}-input-price`}>Input / million</label><input
    id={`${idPrefix}-input-price`}
    bind:value={inputPrice}
    inputmode="decimal"
    placeholder="2.50"
    disabled={!canEdit}
  />
</div>
<div class="form-field">
  <label for={`${idPrefix}-cached-input-price`}>
    Cached input / million
  </label><input
    id={`${idPrefix}-cached-input-price`}
    bind:value={cachedInputPrice}
    inputmode="decimal"
    placeholder="0.25"
    disabled={!canEdit}
  /><small>Leave empty to bill cached tokens at the full input rate.</small>
</div>
<div class="form-field">
  <label for={`${idPrefix}-cache-write-price`}>
    Cache write / million
  </label><input
    id={`${idPrefix}-cache-write-price`}
    bind:value={cacheWritePrice}
    inputmode="decimal"
    placeholder="3.00"
    disabled={!canEdit}
  /><small>Leave empty to bill cache writes at the full input rate.</small>
</div>
<div class="form-field">
  <label for={`${idPrefix}-cache-write-5m-price`}>
    Cache write 5m / million
  </label><input
    id={`${idPrefix}-cache-write-5m-price`}
    bind:value={cacheWrite5mPrice}
    inputmode="decimal"
    placeholder="3.00"
    disabled={!canEdit}
  /><small>Defaults to the cache write rate, then input.</small>
</div>
<div class="form-field">
  <label for={`${idPrefix}-cache-write-1h-price`}>
    Cache write 1h / million
  </label><input
    id={`${idPrefix}-cache-write-1h-price`}
    bind:value={cacheWrite1hPrice}
    inputmode="decimal"
    placeholder="6.00"
    disabled={!canEdit}
  /><small>Defaults to the cache write rate, then input.</small>
</div>
<div class="form-field">
  <label for={`${idPrefix}-output-price`}>Output / million</label><input
    id={`${idPrefix}-output-price`}
    bind:value={outputPrice}
    inputmode="decimal"
    placeholder="10.00"
    disabled={!canEdit}
  />
</div>
<div class="form-field">
  <label for={`${idPrefix}-unit-price`}>Media unit price</label><input
    id={`${idPrefix}-unit-price`}
    bind:value={unitPrice}
    inputmode="decimal"
    placeholder="0.04"
    disabled={!canEdit}
  />
</div>
<div class="form-field">
  <label for={`${idPrefix}-audio-input-price`}>
    Audio input / million
  </label><input
    id={`${idPrefix}-audio-input-price`}
    bind:value={audioInputPrice}
    inputmode="decimal"
    placeholder="4.00"
    disabled={!canEdit}
  />
</div>
<div class="form-field">
  <label for={`${idPrefix}-cached-audio-input-price`}>
    Cached audio input / million
  </label><input
    id={`${idPrefix}-cached-audio-input-price`}
    bind:value={cachedAudioInputPrice}
    inputmode="decimal"
    placeholder="0.50"
    disabled={!canEdit}
  /><small>Defaults to cached input, then input.</small>
</div>
<div class="form-field">
  <label for={`${idPrefix}-audio-output-price`}>
    Audio output / million
  </label><input
    id={`${idPrefix}-audio-output-price`}
    bind:value={audioOutputPrice}
    inputmode="decimal"
    placeholder="16.00"
    disabled={!canEdit}
  /><small>Defaults to the output rate.</small>
</div>
<div class="form-field">
  <label for={`${idPrefix}-image-input-price`}>
    Image input / million
  </label><input
    id={`${idPrefix}-image-input-price`}
    bind:value={imageInputPrice}
    inputmode="decimal"
    placeholder="6.00"
    disabled={!canEdit}
  /><small>Defaults to the input rate.</small>
</div>
<div class="form-field">
  <label for={`${idPrefix}-cached-image-input-price`}>
    Cached image input / million
  </label><input
    id={`${idPrefix}-cached-image-input-price`}
    bind:value={cachedImageInputPrice}
    inputmode="decimal"
    placeholder="0.75"
    disabled={!canEdit}
  /><small>Defaults to cached input, then input.</small>
</div>
<div class="form-field">
  <label for={`${idPrefix}-image-output-price`}>
    Image output / million
  </label><input
    id={`${idPrefix}-image-output-price`}
    bind:value={imageOutputPrice}
    inputmode="decimal"
    placeholder="24.00"
    disabled={!canEdit}
  /><small>Defaults to the output rate.</small>
</div>
<div class="form-field">
  <label for={`${idPrefix}-per-request-price`}>Per-request fee</label><input
    id={`${idPrefix}-per-request-price`}
    bind:value={perRequestPrice}
    inputmode="decimal"
    placeholder="0.01"
    disabled={!canEdit}
  /><small>Charged once per successful attempt.</small>
</div>
<div class="form-field">
  <label for={`${idPrefix}-training-price`}>Training / million tokens</label>
  <input
    id={`${idPrefix}-training-price`}
    bind:value={trainingPrice}
    inputmode="decimal"
    placeholder="8.00"
    disabled={!canEdit}
  />
</div>
<div class="form-field">
  <label for={`${idPrefix}-storage-price`}>Storage / GiB-day</label>
  <input
    id={`${idPrefix}-storage-price`}
    bind:value={storagePrice}
    inputmode="decimal"
    placeholder="0.10"
    disabled={!canEdit}
  />
  <small>One GiB is 1,073,741,824 bytes.</small>
</div>
<div class="form-field">
  <label for={`${idPrefix}-cache-storage-price`}>
    Cache storage / million token-hours
  </label>
  <input
    id={`${idPrefix}-cache-storage-price`}
    bind:value={cacheStoragePrice}
    inputmode="decimal"
    placeholder="1.00"
    disabled={!canEdit}
  />
</div>
<div class="form-field">
  <label for={`${idPrefix}-batch-multiplier`}>Batch multiplier</label><input
    id={`${idPrefix}-batch-multiplier`}
    bind:value={batchMultiplier}
    inputmode="decimal"
    placeholder="0.50"
    disabled={!canEdit}
  /><small>
    Applies to usage marked batch; batch usage without a multiplier is unpriced.
  </small>
</div>
<PriceExtraFields
  {idPrefix}
  {canEdit}
  {rateFields}
  bind:containerFees
  bind:toolFees
  bind:tiers
  bind:windowZone
  bind:windowPeriods
/>
