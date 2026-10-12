<script lang="ts">
  import { useMutationIntent } from '$lib/forms/mutationIntent.svelte';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import CursorPagination from '$lib/components/CursorPagination.svelte';
  import ReadOnlyNote from '$lib/components/ReadOnlyNote.svelte';
  import {
    cursorPaginationProps,
    emptyCursorHistory,
    resetCursor
  } from '$lib/lists/pagination';
  import { errorMessage } from '$lib/api/http';
  import { dateTimeLocalValue, formatDate } from '$lib/format';
  import { useServiceCapabilities } from '$lib/features/access/session/serviceCapabilities.svelte';
  import { useRole } from '$lib/features/access/session/useRole.svelte';
  import { listProviderKinds } from '$lib/features/providers/api/models';
  import {
    listProviderVendors,
    type ProviderKind
  } from '$lib/features/providers/api/providers';
  import { providerKeys } from '$lib/features/providers/providerKeys';
  import { operationKinds } from './history/api';
  import {
    createPricingRevision,
    listPricing,
    type PriceDraft,
    type PricingRevision
  } from './api/pricing';
  import { pricingKeys } from './pricingKeys';
  import {
    decimalText,
    optionalDecimal,
    tierRows,
    toolFeeRows,
    windowsDraft
  } from './pricingValidation';

  const createIntent = useMutationIntent();

  // One compact line for the components beyond the per-million table columns.
  function extraComponents(price: PricingRevision['prices'][number]): string {
    const parts: string[] = [];
    for (const [key, label] of [
      ['audio_input_per_million', 'audio in'],
      ['cached_audio_input_per_million', 'cached audio'],
      ['audio_output_per_million', 'audio out'],
      ['image_input_per_million', 'image in'],
      ['cached_image_input_per_million', 'cached image'],
      ['image_output_per_million', 'image out'],
      ['per_request', 'per request'],
      ['batch_multiplier', 'batch']
    ] as const) {
      if ((price as Record<string, unknown>)[key] != null)
        parts.push(`${label} ${(price as Record<string, unknown>)[key]}`);
    }
    if (price.tool_calls && Object.keys(price.tool_calls).length)
      parts.push(`tools ${Object.keys(price.tool_calls).join(', ')}`);
    if (price.tiers?.length) parts.push(`${price.tiers.length} tiers`);
    if (price.windows?.time_zone)
      parts.push(`windows ${price.windows.time_zone}`);
    return parts.join(' · ');
  }

  let {
    blocked,
    onBusyChange,
    onFeedback
  }: {
    blocked: boolean;
    onBusyChange: (busy: boolean) => void;
    onFeedback: (feedback: { status: string; error: string }) => void;
  } = $props();

  const queryClient = useQueryClient();
  const services = useServiceCapabilities();
  const access = useRole();
  const canEditPricing = $derived(access.can('pricing.update'));
  let providerKind = $state<ProviderKind | null>(null);
  let providerId = $state('');
  let vendorId = $state('');
  const providerVendors = createQuery(() => ({
    queryKey: providerKeys.vendors(),
    enabled: services.gatewayAvailable,
    queryFn: () => listProviderVendors()
  }));
  let model = $state('');
  let operation = $state<(typeof operationKinds)[number]>('generation');
  let inputPrice = $state('');
  let cachedInputPrice = $state('');
  let cacheWritePrice = $state('');
  let cacheWrite5mPrice = $state('');
  let cacheWrite1hPrice = $state('');
  let outputPrice = $state('');
  let unitPrice = $state('');
  let audioInputPrice = $state('');
  let cachedAudioInputPrice = $state('');
  let audioOutputPrice = $state('');
  let imageInputPrice = $state('');
  let cachedImageInputPrice = $state('');
  let imageOutputPrice = $state('');
  let perRequestPrice = $state('');
  let batchMultiplier = $state('');
  let toolFees = $state<{ name: string; fee: string }[]>([]);
  let tiers = $state<{ threshold: string; rates: Record<string, string> }[]>(
    []
  );
  let windowZone = $state('');
  let windowPeriods = $state<
    { days: number[]; start: string; end: string; multiplier: string }[]
  >([]);
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
  const rateFields = $derived([
    ['input_per_million', inputPrice],
    ['cached_input_per_million', cachedInputPrice],
    ['cache_write_input_per_million', cacheWritePrice],
    ['cache_write_5m_input_per_million', cacheWrite5mPrice],
    ['cache_write_1h_input_per_million', cacheWrite1hPrice],
    ['output_per_million', outputPrice],
    ['audio_input_per_million', audioInputPrice],
    ['cached_audio_input_per_million', cachedAudioInputPrice],
    ['audio_output_per_million', audioOutputPrice],
    ['image_input_per_million', imageInputPrice],
    ['cached_image_input_per_million', cachedImageInputPrice],
    ['image_output_per_million', imageOutputPrice]
  ] as const);
  let currency = $state('USD');
  let effectiveAt = $state(dateTimeLocalValue(new Date()));
  let savingPrice = $state(false);
  const pricingPagination = $state(emptyCursorHistory());

  const providerKinds = createQuery(() => ({
    queryKey: providerKeys.kinds(),
    enabled: services.gatewayAvailable,
    queryFn: ({ signal }) => listProviderKinds(signal)
  }));

  $effect(() => {
    if (!providerKind && providerKinds.data?.[0])
      providerKind = providerKinds.data[0].kind;
  });

  const pricing = createQuery(() => {
    // The page a key names, so a fetch never stores another page under it.
    const cursor = pricingPagination.cursor;
    return {
      queryKey: pricingKeys.page(cursor),
      enabled: services.limitsEnforced,
      queryFn: () => listPricing(cursor)
    };
  });

  async function addPricing(event: SubmitEvent) {
    event.preventDefault();
    if (!canEditPricing || savingPrice || blocked) return;
    onFeedback({ status: '', error: '' });
    if (!providerKind || !model.trim() || !operation.trim()) {
      onFeedback({
        status: '',
        error: 'Provider kind, model, and operation are required.'
      });
      return;
    }
    const submitted = {
      providerKind,
      vendorId,
      providerId,
      model,
      operation,
      inputPrice,
      cachedInputPrice,
      cacheWritePrice,
      cacheWrite5mPrice,
      cacheWrite1hPrice,
      outputPrice,
      unitPrice,
      audioInputPrice,
      cachedAudioInputPrice,
      audioOutputPrice,
      imageInputPrice,
      cachedImageInputPrice,
      imageOutputPrice,
      perRequestPrice,
      batchMultiplier,
      toolFees,
      tiers,
      windowZone,
      windowPeriods,
      currency,
      effectiveAt
    };
    savingPrice = true;
    onBusyChange(true);
    let committed = false;
    try {
      const price: PriceDraft = {
        provider_kind: submitted.providerKind,
        vendor_id: submitted.vendorId || null,
        provider_id: submitted.providerId || null,
        model: submitted.model.trim(),
        operation: submitted.operation,
        input_per_million: optionalDecimal(submitted.inputPrice),
        cached_input_per_million: optionalDecimal(submitted.cachedInputPrice),
        cache_write_input_per_million: optionalDecimal(
          submitted.cacheWritePrice
        ),
        cache_write_5m_input_per_million: optionalDecimal(
          submitted.cacheWrite5mPrice
        ),
        cache_write_1h_input_per_million: optionalDecimal(
          submitted.cacheWrite1hPrice
        ),
        output_per_million: optionalDecimal(submitted.outputPrice),
        unit_price: optionalDecimal(submitted.unitPrice),
        audio_input_per_million: optionalDecimal(submitted.audioInputPrice),
        cached_audio_input_per_million: optionalDecimal(
          submitted.cachedAudioInputPrice
        ),
        audio_output_per_million: optionalDecimal(submitted.audioOutputPrice),
        image_input_per_million: optionalDecimal(submitted.imageInputPrice),
        cached_image_input_per_million: optionalDecimal(
          submitted.cachedImageInputPrice
        ),
        image_output_per_million: optionalDecimal(submitted.imageOutputPrice),
        per_request: optionalDecimal(submitted.perRequestPrice),
        batch_multiplier: optionalDecimal(submitted.batchMultiplier),
        tool_calls: toolFeeRows(submitted.toolFees),
        tiers: tierRows(
          submitted.tiers,
          rateFields
            .filter(([, value]) => decimalText(value) != null)
            .map(([key]) => key)
        ),
        windows: windowsDraft(submitted.windowZone, submitted.windowPeriods),
        currency: submitted.currency.trim().toUpperCase()
      };
      if (
        !price.input_per_million &&
        !price.output_per_million &&
        !price.unit_price &&
        !price.per_request &&
        !price.audio_input_per_million &&
        !price.audio_output_per_million &&
        !price.image_input_per_million &&
        !price.image_output_per_million &&
        !price.tool_calls
      )
        throw new Error('Enter at least one price.');
      const submission = createIntent.prepare({
        effectiveAt: new Date(submitted.effectiveAt).toISOString(),
        prices: [price]
      });
      await createPricingRevision(
        submission.input.effectiveAt,
        submission.input.prices,
        submission.requestId
      );
      createIntent.reset();
      committed = true;
      onFeedback({
        status:
          'Pricing revision created. New usage will use the effective revision.',
        error: ''
      });
      if (model === submitted.model) model = '';
      if (inputPrice === submitted.inputPrice) inputPrice = '';
      if (cachedInputPrice === submitted.cachedInputPrice)
        cachedInputPrice = '';
      if (cacheWritePrice === submitted.cacheWritePrice) cacheWritePrice = '';
      if (cacheWrite5mPrice === submitted.cacheWrite5mPrice)
        cacheWrite5mPrice = '';
      if (cacheWrite1hPrice === submitted.cacheWrite1hPrice)
        cacheWrite1hPrice = '';
      if (outputPrice === submitted.outputPrice) outputPrice = '';
      if (unitPrice === submitted.unitPrice) unitPrice = '';
      if (audioInputPrice === submitted.audioInputPrice) audioInputPrice = '';
      if (cachedAudioInputPrice === submitted.cachedAudioInputPrice)
        cachedAudioInputPrice = '';
      if (audioOutputPrice === submitted.audioOutputPrice)
        audioOutputPrice = '';
      if (imageInputPrice === submitted.imageInputPrice) imageInputPrice = '';
      if (cachedImageInputPrice === submitted.cachedImageInputPrice)
        cachedImageInputPrice = '';
      if (imageOutputPrice === submitted.imageOutputPrice)
        imageOutputPrice = '';
      if (perRequestPrice === submitted.perRequestPrice) perRequestPrice = '';
      if (batchMultiplier === submitted.batchMultiplier) batchMultiplier = '';
      toolFees = [];
      tiers = [];
      windowZone = '';
      windowPeriods = [];
      if (providerId === submitted.providerId) providerId = '';
      if (effectiveAt === submitted.effectiveAt)
        effectiveAt = dateTimeLocalValue(new Date());
      resetCursor(pricingPagination);
      // The query observes the first page only once its options update, so
      // mark every cached page stale rather than refetching the current one.
      await queryClient.invalidateQueries({ queryKey: pricingKeys.pages() });
    } catch (cause) {
      onFeedback({
        status: committed ? 'Pricing revision created.' : '',
        error: committed
          ? `Pricing revision created, but refreshing failed. ${errorMessage(cause)}`
          : errorMessage(cause, 'The pricing revision could not be created.')
      });
    } finally {
      savingPrice = false;
      onBusyChange(false);
    }
  }
</script>

{#if services.gatewayAvailable}
  <section class="settings-section" aria-labelledby="pricing-title">
    <div class="section-heading">
      <div>
        <p class="eyebrow">Cost estimates</p>
        <h2 id="pricing-title">Pricing revisions</h2>
        <p>
          Prices are exact decimals. A missing price remains visibly unpriced
          and is never treated as zero. An empty cached input rate bills cached
          tokens at the full input rate.
        </p>
      </div>
    </div>
    {#if !canEditPricing}<ReadOnlyNote
        >Your role can view pricing revisions but not create them.</ReadOnlyNote
      >{/if}
    <form class="card price-form" onsubmit={addPricing}>
      <div class="form-grid">
        <div class="form-field">
          <label for="provider-kind">Provider kind</label><select
            id="provider-kind"
            bind:value={providerKind}
            onchange={() => {
              vendorId = '';
            }}
            disabled={!canEditPricing ||
              providerKinds.isPending ||
              providerKinds.isError}
            >{#each providerKinds.data ?? [] as option (option.kind)}<option
                value={option.kind}>{option.label}</option
              >{/each}</select
          >{#if providerKinds.isPending}<small
              >Loading provider capabilities…</small
            >{:else if providerKinds.isError}<small class="inline-problem"
              >Provider capabilities are unavailable; pricing changes are
              disabled.</small
            >{/if}
        </div>
        <div class="form-field">
          <label for="price-vendor">Vendor scope</label><select
            id="price-vendor"
            bind:value={vendorId}
            disabled={!canEditPricing}
            ><option value="">All vendors using this connector</option
            >{#each (providerVendors.data ?? []).filter((vendor) => vendor.connector === providerKind) as vendor (vendor.id)}<option
                value={vendor.id}>{vendor.name}</option
              >{/each}</select
          >
        </div>
        <div class="form-field">
          <label for="provider-id">Provider ID override</label><input
            id="provider-id"
            bind:value={providerId}
            class="mono"
            placeholder="Optional UUID"
            disabled={!canEditPricing}
          />
        </div>
        <div class="form-field">
          <label for="price-model">Upstream model</label><input
            id="price-model"
            bind:value={model}
            required
            disabled={!canEditPricing}
          />
        </div>
        <div class="form-field">
          <label for="price-operation">Operation</label><select
            id="price-operation"
            bind:value={operation}
            disabled={!canEditPricing}
            >{#each operationKinds as option (option)}<option value={option}
                >{option}</option
              >{/each}</select
          >
        </div>
        <div class="form-field">
          <label for="input-price">Input / million</label><input
            id="input-price"
            bind:value={inputPrice}
            inputmode="decimal"
            placeholder="2.50"
            disabled={!canEditPricing}
          />
        </div>
        <div class="form-field">
          <label for="cached-input-price">Cached input / million</label><input
            id="cached-input-price"
            bind:value={cachedInputPrice}
            inputmode="decimal"
            placeholder="0.25"
            disabled={!canEditPricing}
          /><small
            >Leave empty to bill cached tokens at the full input rate.</small
          >
        </div>
        <div class="form-field">
          <label for="cache-write-price">Cache write / million</label><input
            id="cache-write-price"
            bind:value={cacheWritePrice}
            inputmode="decimal"
            placeholder="3.00"
            disabled={!canEditPricing}
          /><small
            >Leave empty to bill cache writes at the full input rate.</small
          >
        </div>
        <div class="form-field">
          <label for="cache-write-5m-price">Cache write 5m / million</label
          ><input
            id="cache-write-5m-price"
            bind:value={cacheWrite5mPrice}
            inputmode="decimal"
            placeholder="3.00"
            disabled={!canEditPricing}
          /><small>Defaults to the cache write rate, then input.</small>
        </div>
        <div class="form-field">
          <label for="cache-write-1h-price">Cache write 1h / million</label
          ><input
            id="cache-write-1h-price"
            bind:value={cacheWrite1hPrice}
            inputmode="decimal"
            placeholder="6.00"
            disabled={!canEditPricing}
          /><small>Defaults to the cache write rate, then input.</small>
        </div>
        <div class="form-field">
          <label for="output-price">Output / million</label><input
            id="output-price"
            bind:value={outputPrice}
            inputmode="decimal"
            placeholder="10.00"
            disabled={!canEditPricing}
          />
        </div>
        <div class="form-field">
          <label for="unit-price">Media unit price</label><input
            id="unit-price"
            bind:value={unitPrice}
            inputmode="decimal"
            placeholder="0.04"
            disabled={!canEditPricing}
          />
        </div>
        <div class="form-field">
          <label for="audio-input-price">Audio input / million</label><input
            id="audio-input-price"
            bind:value={audioInputPrice}
            inputmode="decimal"
            placeholder="4.00"
            disabled={!canEditPricing}
          />
        </div>
        <div class="form-field">
          <label for="cached-audio-input-price"
            >Cached audio input / million</label
          ><input
            id="cached-audio-input-price"
            bind:value={cachedAudioInputPrice}
            inputmode="decimal"
            placeholder="0.50"
            disabled={!canEditPricing}
          /><small>Defaults to cached input, then input.</small>
        </div>
        <div class="form-field">
          <label for="audio-output-price">Audio output / million</label><input
            id="audio-output-price"
            bind:value={audioOutputPrice}
            inputmode="decimal"
            placeholder="16.00"
            disabled={!canEditPricing}
          /><small>Defaults to the output rate.</small>
        </div>
        <div class="form-field">
          <label for="image-input-price">Image input / million</label><input
            id="image-input-price"
            bind:value={imageInputPrice}
            inputmode="decimal"
            placeholder="6.00"
            disabled={!canEditPricing}
          /><small>Defaults to the input rate.</small>
        </div>
        <div class="form-field">
          <label for="cached-image-input-price"
            >Cached image input / million</label
          ><input
            id="cached-image-input-price"
            bind:value={cachedImageInputPrice}
            inputmode="decimal"
            placeholder="0.75"
            disabled={!canEditPricing}
          /><small>Defaults to cached input, then input.</small>
        </div>
        <div class="form-field">
          <label for="image-output-price">Image output / million</label><input
            id="image-output-price"
            bind:value={imageOutputPrice}
            inputmode="decimal"
            placeholder="24.00"
            disabled={!canEditPricing}
          /><small>Defaults to the output rate.</small>
        </div>
        <div class="form-field">
          <label for="per-request-price">Per-request fee</label><input
            id="per-request-price"
            bind:value={perRequestPrice}
            inputmode="decimal"
            placeholder="0.01"
            disabled={!canEditPricing}
          /><small>Charged once per successful attempt.</small>
        </div>
        <div class="form-field">
          <label for="batch-multiplier">Batch multiplier</label><input
            id="batch-multiplier"
            bind:value={batchMultiplier}
            inputmode="decimal"
            placeholder="0.50"
            disabled={!canEditPricing}
          /><small
            >Applies to usage marked batch; batch usage without a multiplier is
            unpriced.</small
          >
        </div>
        <div class="form-field full">
          <label for="currency">Currency</label><input
            id="currency"
            bind:value={currency}
            maxlength="3"
            required
            disabled={!canEditPricing}
          />
        </div>
        <div class="form-field full">
          <label for="effective-at">Effective at</label><input
            id="effective-at"
            bind:value={effectiveAt}
            type="datetime-local"
            required
            disabled={!canEditPricing}
          />
        </div>
      </div>
      <fieldset class="extra-components" disabled={!canEditPricing}>
        <legend>Additional components</legend>
        <p class="muted">
          Tool fees bill each invocation; tiers replace the per-million rate set
          above an input threshold; windows multiply the charge by time of day.
        </p>
        <div class="component-rows" aria-label="Tool fees">
          {#each toolFees as row, index (index)}
            <div class="component-row">
              <label class="sr-only" for={`tool-name-${index}`}>Tool name</label
              >
              <input
                id={`tool-name-${index}`}
                bind:value={row.name}
                placeholder="web_search"
                class="mono"
              />
              <label class="sr-only" for={`tool-fee-${index}`}>Fee</label>
              <input
                id={`tool-fee-${index}`}
                bind:value={row.fee}
                inputmode="decimal"
                placeholder="0.010"
              />
              <button
                type="button"
                class="text-button"
                onclick={() => {
                  toolFees = toolFees.filter((_, i) => i !== index);
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
              <label for={`tier-threshold-${index}`}>Above input tokens</label>
              <input
                id={`tier-threshold-${index}`}
                bind:value={row.threshold}
                inputmode="numeric"
                placeholder="1000000"
              />
              {#each rateFields.filter(([, value]) => value.trim() !== '') as [field, parentValue] (field)}
                <label class="sr-only" for={`tier-${index}-${field}`}
                  >{field.replaceAll('_', ' ')}</label
                >
                <input
                  id={`tier-${index}-${field}`}
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
                  tiers = tiers.filter((_, i) => i !== index);
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
            <label for="window-zone">Window time zone</label>
            <input
              id="window-zone"
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
                        ? period.days.filter((d) => d !== value)
                        : [...period.days, value];
                    }}>{label}</button
                  >
                {/each}
              </div>
              <label class="sr-only" for={`window-start-${index}`}>Start</label>
              <input
                id={`window-start-${index}`}
                bind:value={period.start}
                placeholder="00:00"
                inputmode="numeric"
              />
              <label class="sr-only" for={`window-end-${index}`}>End</label>
              <input
                id={`window-end-${index}`}
                bind:value={period.end}
                placeholder="24:00"
                inputmode="numeric"
              />
              <label class="sr-only" for={`window-multiplier-${index}`}
                >Multiplier</label
              >
              <input
                id={`window-multiplier-${index}`}
                bind:value={period.multiplier}
                inputmode="decimal"
                placeholder="0.75"
              />
              <button
                type="button"
                class="text-button"
                onclick={() => {
                  windowPeriods = windowPeriods.filter((_, i) => i !== index);
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
      <button
        class="button button-primary"
        type="submit"
        disabled={!canEditPricing ||
          !services.limitsEnforced ||
          blocked ||
          savingPrice ||
          !providerKind ||
          providerKinds.isError}
        >{savingPrice ? 'Creating…' : 'Create pricing revision'}</button
      >
    </form>

    {#if services.pending}<div class="loading-state" role="status">
        Loading installation capabilities…
      </div>
    {:else if services.error}<div class="inline-problem" role="alert">
        Installation capabilities are unavailable, so pricing revisions cannot
        be listed.
        <button
          class="text-button"
          type="button"
          onclick={() => services.retry()}>Try again</button
        >
      </div>
    {:else if !services.limitsEnforced}<div
        class="card empty-state"
        role="status"
      >
        Pricing revisions become available once limits and accounting are
        enforced by this installation.
      </div>
    {:else if pricing.isPending}<div class="loading-state" role="status">
        Loading revisions…
      </div>
    {:else if pricing.isError}<div class="inline-problem" role="alert">
        {errorMessage(pricing.error)}
        <button class="text-button" onclick={() => pricing.refetch()}
          >Try again</button
        >
      </div>
    {:else if pricing.data?.items.length === 0 && pricingPagination.history.length === 0}<div
        class="card empty-state"
      >
        No pricing revisions. Usage cost will be marked unpriced.
      </div>
    {:else}
      <div class="revision-list">
        {#each pricing.data?.items ?? [] as revision (revision.id)}
          <details class="card">
            <summary
              ><span
                ><strong>Revision {revision.revision}</strong><small
                  >Effective {formatDate(revision.effective_at)}</small
                ><small
                  >Created {formatDate(revision.created_at)} by
                  <span class="mono">{revision.created_by}</span></small
                >{#if revision.source_name}<small
                    >Published from
                    <span class="mono">{revision.source_name}</span></small
                  >{/if}</span
              ><span class="badge">{revision.prices.length} entries</span
              ></summary
            >
            <!-- svelte-ignore a11y_no_noninteractive_tabindex -->
            <div
              class="table-shell"
              tabindex="0"
              role="region"
              aria-label={`Prices in revision ${revision.revision}`}
            >
              <table class="data-table">
                <caption class="sr-only"
                  >Prices in revision {revision.revision}</caption
                >
                <thead
                  ><tr
                    ><th scope="col">Provider / model</th><th scope="col"
                      >Operation</th
                    ><th scope="col">Input / million</th><th scope="col"
                      >Cached input / million</th
                    ><th scope="col">Cache write / million</th><th scope="col"
                      >Write 5m / million</th
                    ><th scope="col">Write 1h / million</th><th scope="col"
                      >Output / million</th
                    ><th scope="col">Unit</th><th scope="col">Components</th><th
                      scope="col">Currency</th
                    ></tr
                  ></thead
                >
                <tbody
                  >{#each revision.prices as price, priceIndex (`${price.provider_kind}:${price.model}:${price.operation}:${priceIndex}`)}<tr
                      ><td
                        ><strong
                          >{price.vendor_id ?? price.provider_kind}</strong
                        >{#if price.provider_id}<small
                            >{price.provider_id}</small
                          >{/if}<small>{price.model}</small></td
                      ><td>{price.operation}</td><td
                        >{price.input_per_million ?? '—'}</td
                      ><td
                        >{price.cached_input_per_million ??
                          'Billed as input'}</td
                      ><td
                        >{price.cache_write_input_per_million ??
                          'Billed as input'}</td
                      ><td>{price.cache_write_5m_input_per_million ?? '—'}</td
                      ><td>{price.cache_write_1h_input_per_million ?? '—'}</td
                      ><td>{price.output_per_million ?? '—'}</td><td
                        >{price.unit_price ?? '—'}</td
                      ><td>{extraComponents(price) || '—'}</td><td
                        >{price.currency}</td
                      ></tr
                    >{/each}</tbody
                >
              </table>
            </div>
          </details>
        {/each}
      </div>
      <CursorPagination
        {...cursorPaginationProps(pricingPagination, pricing.data?.nextCursor)}
        label="Pricing revision pages"
      />
    {/if}
  </section>
{/if}

<style>
  .settings-section {
    margin-top: 2.5rem;
  }
  .section-heading {
    margin-bottom: 1rem;
  }
  h2 {
    margin: 0;
    font-size: 1rem;
    font-weight: 500;
    letter-spacing: -0.02em;
  }
  .section-heading p:last-child {
    max-width: 44rem;
    margin: 0.5rem 0 0;
    color: var(--foreground-muted);
    line-height: 1.5;
  }
  .price-form {
    padding: 1.5rem;
  }
  .price-form .button {
    margin-top: 1rem;
  }
  .form-field small {
    display: block;
    margin-top: 0.25rem;
    font-size: var(--text-caption);
  }
  summary .mono {
    font-size: var(--text-caption);
    overflow-wrap: anywhere;
  }
  .revision-list {
    display: grid;
    gap: 0.75rem;
    margin-top: 1rem;
  }
  details {
    overflow: hidden;
  }
  /* The price table sits flush inside its revision card; the hairline on top
     separates it from the summary instead of stacking a second frame. */
  details .table-shell {
    border: 0;
    border-top: 1px solid var(--border-hairline);
    border-radius: 0;
  }
  summary {
    display: flex;
    min-height: 3.5rem;
    align-items: center;
    justify-content: space-between;
    gap: 1rem;
    padding: 0.75rem 1.25rem;
    cursor: pointer;
  }
  summary strong,
  summary small,
  td strong,
  td small {
    display: block;
  }
  summary small,
  td small {
    color: var(--foreground-muted);
  }
  @media (max-width: 36rem) {
    .price-form {
      padding: 1rem;
    }
  }
</style>
