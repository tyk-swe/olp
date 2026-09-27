<script lang="ts">
  import { createQuery } from '@tanstack/svelte-query';
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
  import { listProviderKinds } from '$lib/features/providers/models';
  import {
    listProviderVendors,
    type ProviderKind
  } from '$lib/features/providers/api';
  import { providerKeys } from '$lib/features/providers/providerKeys';
  import { operationKinds } from './history/api';
  import {
    createPricingRevision,
    listPricing,
    type PriceDraft
  } from './pricing';
  import { pricingKeys } from './pricingKeys';
  import { optionalDecimal } from './pricingValidation';

  let {
    blocked,
    onBusyChange,
    onFeedback
  }: {
    blocked: boolean;
    onBusyChange: (busy: boolean) => void;
    onFeedback: (feedback: { status: string; error: string }) => void;
  } = $props();

  const services = useServiceCapabilities();
  const access = useRole();
  const canEditPricing = $derived(access.can('pricing.update'));
  let providerKind = $state<ProviderKind | null>(null);
  let providerId = $state('');
  let vendorId = $state('');
  const providerVendors = createQuery(() => ({
    queryKey: ['provider-vendors'],
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

  const pricing = createQuery(() => ({
    queryKey: pricingKeys.page(pricingPagination.cursor),
    enabled: services.limitsEnforced,
    queryFn: () => listPricing(pricingPagination.cursor)
  }));

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
      currency,
      effectiveAt
    };
    savingPrice = true;
    onBusyChange(true);
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
        currency: submitted.currency.trim().toUpperCase()
      };
      if (
        !price.input_per_million &&
        !price.output_per_million &&
        !price.unit_price
      )
        throw new Error('Enter at least one price.');
      await createPricingRevision(
        new Date(submitted.effectiveAt).toISOString(),
        [price]
      );
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
      if (providerId === submitted.providerId) providerId = '';
      if (effectiveAt === submitted.effectiveAt)
        effectiveAt = dateTimeLocalValue(new Date());
      resetCursor(pricingPagination);
      await pricing.refetch();
    } catch (cause) {
      onFeedback({
        status: '',
        error: errorMessage(cause, 'The pricing revision could not be created.')
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
                    ><th scope="col">Unit</th><th scope="col">Currency</th></tr
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
                      ><td>{price.currency}</td></tr
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
