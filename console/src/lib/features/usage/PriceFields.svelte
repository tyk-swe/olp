<script lang="ts">
  import {
    rateFieldPairs,
    type TierRow,
    type ToolFeeRow,
    type WindowPeriodRow
  } from './priceEntry';

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
    batchMultiplier = $bindable(''),
    toolFees = $bindable<ToolFeeRow[]>([]),
    tiers = $bindable<TierRow[]>([]),
    windowZone = $bindable(''),
    windowPeriods = $bindable<WindowPeriodRow[]>([])
  } = $props<{
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
    batchMultiplier?: string;
    toolFees?: ToolFeeRow[];
    tiers?: TierRow[];
    windowZone?: string;
    windowPeriods?: WindowPeriodRow[];
  }>();

  const weekDays = [
    ['1', 'Mon'],
    ['2', 'Tue'],
    ['3', 'Wed'],
    ['4', 'Thu'],
    ['5', 'Fri'],
    ['6', 'Sat'],
    ['7', 'Sun']
  ] as const;

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
  <label for={`${idPrefix}-cached-input-price`}>Cached input / million</label
  ><input
    id={`${idPrefix}-cached-input-price`}
    bind:value={cachedInputPrice}
    inputmode="decimal"
    placeholder="0.25"
    disabled={!canEdit}
  /><small>Leave empty to bill cached tokens at the full input rate.</small>
</div>
<div class="form-field">
  <label for={`${idPrefix}-cache-write-price`}>Cache write / million</label
  ><input
    id={`${idPrefix}-cache-write-price`}
    bind:value={cacheWritePrice}
    inputmode="decimal"
    placeholder="3.00"
    disabled={!canEdit}
  /><small>Leave empty to bill cache writes at the full input rate.</small>
</div>
<div class="form-field">
  <label for={`${idPrefix}-cache-write-5m-price`}
    >Cache write 5m / million</label
  ><input
    id={`${idPrefix}-cache-write-5m-price`}
    bind:value={cacheWrite5mPrice}
    inputmode="decimal"
    placeholder="3.00"
    disabled={!canEdit}
  /><small>Defaults to the cache write rate, then input.</small>
</div>
<div class="form-field">
  <label for={`${idPrefix}-cache-write-1h-price`}
    >Cache write 1h / million</label
  ><input
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
  <label for={`${idPrefix}-audio-input-price`}>Audio input / million</label
  ><input
    id={`${idPrefix}-audio-input-price`}
    bind:value={audioInputPrice}
    inputmode="decimal"
    placeholder="4.00"
    disabled={!canEdit}
  />
</div>
<div class="form-field">
  <label for={`${idPrefix}-cached-audio-input-price`}
    >Cached audio input / million</label
  ><input
    id={`${idPrefix}-cached-audio-input-price`}
    bind:value={cachedAudioInputPrice}
    inputmode="decimal"
    placeholder="0.50"
    disabled={!canEdit}
  /><small>Defaults to cached input, then input.</small>
</div>
<div class="form-field">
  <label for={`${idPrefix}-audio-output-price`}>Audio output / million</label
  ><input
    id={`${idPrefix}-audio-output-price`}
    bind:value={audioOutputPrice}
    inputmode="decimal"
    placeholder="16.00"
    disabled={!canEdit}
  /><small>Defaults to the output rate.</small>
</div>
<div class="form-field">
  <label for={`${idPrefix}-image-input-price`}>Image input / million</label
  ><input
    id={`${idPrefix}-image-input-price`}
    bind:value={imageInputPrice}
    inputmode="decimal"
    placeholder="6.00"
    disabled={!canEdit}
  /><small>Defaults to the input rate.</small>
</div>
<div class="form-field">
  <label for={`${idPrefix}-cached-image-input-price`}
    >Cached image input / million</label
  ><input
    id={`${idPrefix}-cached-image-input-price`}
    bind:value={cachedImageInputPrice}
    inputmode="decimal"
    placeholder="0.75"
    disabled={!canEdit}
  /><small>Defaults to cached input, then input.</small>
</div>
<div class="form-field">
  <label for={`${idPrefix}-image-output-price`}>Image output / million</label
  ><input
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
  <label for={`${idPrefix}-batch-multiplier`}>Batch multiplier</label><input
    id={`${idPrefix}-batch-multiplier`}
    bind:value={batchMultiplier}
    inputmode="decimal"
    placeholder="0.50"
    disabled={!canEdit}
  /><small
    >Applies to usage marked batch; batch usage without a multiplier is
    unpriced.</small
  >
</div>
<fieldset class="extra-components" disabled={!canEdit}>
  <legend>Additional components</legend>
  <p class="muted">
    Tool fees bill each invocation; tiers replace the per-million rate set above
    an input threshold; windows multiply the charge by time of day.
  </p>
  <div class="component-rows" aria-label="Tool fees">
    {#each toolFees as row, index (index)}
      <div class="component-row">
        <label class="sr-only" for={`${idPrefix}-tool-name-${index}`}
          >Tool name</label
        >
        <input
          id={`${idPrefix}-tool-name-${index}`}
          bind:value={row.name}
          placeholder="web_search"
          class="mono"
        />
        <label class="sr-only" for={`${idPrefix}-tool-fee-${index}`}>Fee</label>
        <input
          id={`${idPrefix}-tool-fee-${index}`}
          bind:value={row.fee}
          inputmode="decimal"
          placeholder="0.010"
        />
        <button
          type="button"
          class="text-button"
          onclick={() => {
            toolFees = toolFees.filter(
              (_: ToolFeeRow, i: number) => i !== index
            );
          }}>Remove</button
        >
      </div>
    {/each}
    <button
      type="button"
      class="text-button"
      onclick={() => {
        toolFees = [...toolFees, { name: '', fee: '' }];
      }}>Add tool fee</button
    >
  </div>
  <div class="component-rows" aria-label="Input-token tiers">
    {#each tiers as row, index (index)}
      <div class="component-row tier-row">
        <label for={`${idPrefix}-tier-threshold-${index}`}
          >Above input tokens</label
        >
        <input
          id={`${idPrefix}-tier-threshold-${index}`}
          bind:value={row.threshold}
          inputmode="numeric"
          placeholder="1000000"
        />
        {#each rateFields.filter(([, value]) => value.trim() !== '') as [field, parentValue] (field)}
          <label class="sr-only" for={`${idPrefix}-tier-${index}-${field}`}
            >{field.replaceAll('_', ' ')}</label
          >
          <input
            id={`${idPrefix}-tier-${index}-${field}`}
            bind:value={row.rates[field]}
            inputmode="decimal"
            placeholder={parentValue}
            title={field.replaceAll('_', ' ')}
          />
        {/each}
        <button
          type="button"
          class="text-button"
          onclick={() => {
            tiers = tiers.filter((_: TierRow, i: number) => i !== index);
          }}>Remove</button
        >
      </div>
    {/each}
    <button
      type="button"
      class="text-button"
      onclick={() => {
        const rates: Record<string, string> = {};
        for (const [field, value] of rateFields)
          if (value.trim() !== '') rates[field] = '';
        tiers = [...tiers, { threshold: '', rates }];
      }}>Add tier</button
    >
  </div>
  <div class="component-rows" aria-label="Time windows">
    <div class="component-row">
      <label for={`${idPrefix}-window-zone`}>Window time zone</label>
      <input
        id={`${idPrefix}-window-zone`}
        bind:value={windowZone}
        class="mono"
        placeholder="America/New_York"
      />
    </div>
    {#each windowPeriods as period, index (index)}
      <div class="component-row window-row">
        <div
          class="weekday-toggles"
          role="group"
          aria-label={`Window ${index + 1} days`}
        >
          {#each weekDays as [day, label] (day)}
            <button
              type="button"
              class:selected={period.days.includes(Number(day))}
              aria-pressed={period.days.includes(Number(day))}
              onclick={() => {
                const value = Number(day);
                period.days = period.days.includes(value)
                  ? period.days.filter((d: number) => d !== value)
                  : [...period.days, value];
              }}>{label}</button
            >
          {/each}
        </div>
        <label class="sr-only" for={`${idPrefix}-window-start-${index}`}
          >Start</label
        >
        <input
          id={`${idPrefix}-window-start-${index}`}
          bind:value={period.start}
          placeholder="00:00"
          inputmode="numeric"
        />
        <label class="sr-only" for={`${idPrefix}-window-end-${index}`}
          >End</label
        >
        <input
          id={`${idPrefix}-window-end-${index}`}
          bind:value={period.end}
          placeholder="24:00"
          inputmode="numeric"
        />
        <label class="sr-only" for={`${idPrefix}-window-multiplier-${index}`}
          >Multiplier</label
        >
        <input
          id={`${idPrefix}-window-multiplier-${index}`}
          bind:value={period.multiplier}
          inputmode="decimal"
          placeholder="0.75"
        />
        <button
          type="button"
          class="text-button"
          onclick={() => {
            windowPeriods = windowPeriods.filter(
              (_: WindowPeriodRow, i: number) => i !== index
            );
          }}>Remove</button
        >
      </div>
    {/each}
    <button
      type="button"
      class="text-button"
      onclick={() => {
        windowPeriods = [
          ...windowPeriods,
          { days: [], start: '', end: '', multiplier: '' }
        ];
      }}>Add window period</button
    >
  </div>
</fieldset>
