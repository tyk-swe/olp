<script lang="ts">
  import ScrollTable from '#lib/components/ScrollTable.svelte';
  import Callout from '#lib/components/Callout.svelte';
  import { notify } from '#lib/components/toasts.svelte.js';
  import { useMutation } from '#lib/editing/action.svelte.js';
  import { captureResult } from '#lib/editing/result.js';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import CursorPagination from '#lib/components/CursorPagination.svelte';
  import PricingRevisionForm from './PricingRevisionForm.svelte';
  import {
    emptyPriceFields,
    hasAnyPrice,
    priceRevisionDraft,
    type PricingRevisionFormValues,
    extraComponents
  } from './priceEntry';
  import ReadOnlyNote from '#lib/components/ReadOnlyNote.svelte';
  import {
    cursorPaginationProps,
    emptyCursorHistory,
    resetCursor
  } from '#lib/listing/pagination.js';
  import { errorMessage } from '#lib/management/http.js';
  import { dateTimeLocalValue, formatDate } from '#lib/format.js';
  import { useServiceCapabilities } from '#lib/session/serviceCapabilities.svelte.js';
  import { useRole } from '#lib/session/useRole.svelte.js';
  import {
    listProviderKinds,
    listProviderVendors,
    providerKeys
  } from '#lib/features/providers/index.js';

  import { createPricingRevision, listPricing } from './pricing';
  import { pricingKeys } from './pricingKeys';

  const create = useMutation();

  let problem = $state('');
  function onFeedback(feedback: { status: string; error: string }) {
    problem = feedback.error;
    if (feedback.status && !feedback.error) notify(feedback.status);
  }

  const queryClient = useQueryClient();
  const services = useServiceCapabilities();
  const access = useRole();
  const canEditPricing = $derived(access.can('pricing.update'));
  const form = $state<PricingRevisionFormValues>({
    providerKind: null,
    vendorId: '',
    providerId: '',
    model: '',
    operation: 'generation',
    currency: 'USD',
    effectiveAt: dateTimeLocalValue(new Date()),
    ...emptyPriceFields()
  });
  const pricingPagination = $state(emptyCursorHistory());

  const providerVendors = createQuery(() => ({
    queryKey: providerKeys.vendors(),
    queryFn: () => listProviderVendors()
  }));
  const providerKinds = createQuery(() => ({
    queryKey: providerKeys.kinds(),
    enabled: services.gatewayAvailable,
    queryFn: ({ signal }) => listProviderKinds(signal)
  }));

  $effect(() => {
    if (!form.providerKind && providerKinds.data?.[0])
      form.providerKind = providerKinds.data[0].kind;
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
    if (!canEditPricing || create.busy) return;
    onFeedback({ status: '', error: '' });
    if (!form.providerKind || !form.model.trim() || !form.operation.trim()) {
      onFeedback({
        status: '',
        error: 'Provider kind, model, and operation are required.'
      });
      return;
    }
    const submitted = { ...form };
    const prepared = captureResult(() => {
      const price = priceRevisionDraft(submitted);
      if (!hasAnyPrice(price)) throw new Error('Enter at least one price.');
      return {
        effectiveAt: new Date(submitted.effectiveAt).toISOString(),
        prices: [price]
      };
    });
    if (!prepared.ok) {
      onFeedback({
        status: '',
        error: errorMessage(
          prepared.error,
          'The pricing revision could not be created.'
        )
      });
      return;
    }
    await create.submit(
      prepared.value,
      (input, requestId) =>
        createPricingRevision(input.effectiveAt, input.prices, requestId),
      async () => {
        onFeedback({
          status:
            'Pricing revision created. New usage will use the effective revision.',
          error: ''
        });
        if (form.model === submitted.model) form.model = '';
        if (form.inputPrice === submitted.inputPrice) form.inputPrice = '';
        if (form.cachedInputPrice === submitted.cachedInputPrice)
          form.cachedInputPrice = '';
        if (form.cacheWritePrice === submitted.cacheWritePrice)
          form.cacheWritePrice = '';
        if (form.cacheWrite5mPrice === submitted.cacheWrite5mPrice)
          form.cacheWrite5mPrice = '';
        if (form.cacheWrite1hPrice === submitted.cacheWrite1hPrice)
          form.cacheWrite1hPrice = '';
        if (form.outputPrice === submitted.outputPrice) form.outputPrice = '';
        if (form.unitPrice === submitted.unitPrice) form.unitPrice = '';
        if (form.audioInputPrice === submitted.audioInputPrice)
          form.audioInputPrice = '';
        if (form.cachedAudioInputPrice === submitted.cachedAudioInputPrice)
          form.cachedAudioInputPrice = '';
        if (form.audioOutputPrice === submitted.audioOutputPrice)
          form.audioOutputPrice = '';
        if (form.imageInputPrice === submitted.imageInputPrice)
          form.imageInputPrice = '';
        if (form.cachedImageInputPrice === submitted.cachedImageInputPrice)
          form.cachedImageInputPrice = '';
        if (form.imageOutputPrice === submitted.imageOutputPrice)
          form.imageOutputPrice = '';
        if (form.perRequestPrice === submitted.perRequestPrice)
          form.perRequestPrice = '';
        if (form.trainingPrice === submitted.trainingPrice)
          form.trainingPrice = '';
        if (form.storagePrice === submitted.storagePrice)
          form.storagePrice = '';
        if (form.cacheStoragePrice === submitted.cacheStoragePrice)
          form.cacheStoragePrice = '';
        form.containerFees = [];
        if (form.batchMultiplier === submitted.batchMultiplier)
          form.batchMultiplier = '';
        form.toolFees = [];
        form.tiers = [];
        form.windowZone = '';
        form.windowPeriods = [];
        if (form.providerId === submitted.providerId) form.providerId = '';
        if (form.effectiveAt === submitted.effectiveAt)
          form.effectiveAt = dateTimeLocalValue(new Date());
        resetCursor(pricingPagination);
        // The query observes the first page only once its options update, so
        // mark every cached page stale rather than refetching the current one.
        await queryClient.invalidateQueries({ queryKey: pricingKeys.pages() });
      },
      {
        failure: (cause, stage) => {
          onFeedback({
            status: stage === 'accept' ? 'Pricing revision created.' : '',
            error:
              stage === 'accept'
                ? `Pricing revision created, but refreshing failed. ${errorMessage(cause)}`
                : errorMessage(
                    cause,
                    'The pricing revision could not be created.'
                  )
          });
          return '';
        }
      }
    );
  }
</script>

{#if services.gatewayAvailable}
  <section class="settings-section" aria-labelledby="pricing-title">
    <div class="section-heading">
      <div>
        <h2 class="sr-only" id="pricing-title">Pricing revisions</h2>
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
    {#if problem}<Callout tone="danger">{problem}</Callout>{/if}
    <PricingRevisionForm
      {form}
      canEdit={canEditPricing}
      busy={create.busy}
      limitsEnforced={services.limitsEnforced}
      kindsPending={providerKinds.isPending}
      kindsError={providerKinds.isError}
      kinds={providerKinds.data ?? []}
      vendors={providerVendors.data ?? []}
      onSubmit={addPricing}
    />

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
            <ScrollTable label={`Prices in revision ${revision.revision}`}>
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
            </ScrollTable>
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
    font-size: var(--text-body);
    font-weight: 500;
    letter-spacing: -0.02em;
  }
  .section-heading p:last-child {
    max-width: 44rem;
    margin: 0.5rem 0 0;
    color: var(--foreground-muted);
    line-height: 1.5;
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
  details :global(.table-shell) {
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
</style>
