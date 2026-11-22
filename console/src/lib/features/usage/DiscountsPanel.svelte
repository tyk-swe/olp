<script lang="ts">
  import ScrollTable from '#lib/components/ScrollTable.svelte';
  import Callout from '#lib/components/Callout.svelte';
  import EmptyState from '#lib/components/EmptyState.svelte';
  import EntityPicker from '#lib/components/EntityPicker.svelte';
  import { providerOptions } from '#lib/features/providers/index.js';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import CursorPagination from '#lib/shell/CursorPagination.svelte';
  import ReadOnlyNote from '#lib/shell/ReadOnlyNote.svelte';
  import {
    cursorPaginationProps,
    emptyCursorHistory
  } from '#lib/listing/pagination.js';
  import { errorMessage } from '#lib/management/http.js';
  import { formatDate } from '#lib/format.js';
  import { useRole } from '#lib/session/useRole.svelte.js';
  import { useServiceCapabilities } from '#lib/session/serviceCapabilities.svelte.js';
  import { useMutation } from '#lib/editing/action.svelte.js';
  import {
    listProviderKinds,
    listProviderVendors,
    providerKeys
  } from '#lib/features/providers/index.js';
  import {
    createDiscount,
    listDiscounts,
    type DiscountDraft
  } from '#lib/features/usage/discounts.js';
  import { discountKeys } from '#lib/features/usage/discountKeys.js';

  const create = useMutation();
  const queryClient = useQueryClient();
  const services = useServiceCapabilities();
  const access = useRole();
  const canEdit = $derived(access.can('pricing.update'));

  let providerKind = $state('');
  let vendorId = $state('');
  let providerId = $state('');
  let multiplier = $state('');
  let effectiveAt = $state('');
  let notice = $state('');
  const pagination = $state(emptyCursorHistory());

  const providerKinds = createQuery(() => ({
    queryKey: providerKeys.kinds(),
    enabled: services.gatewayAvailable,
    queryFn: ({ signal }) => listProviderKinds(signal)
  }));
  const providerVendors = createQuery(() => ({
    queryKey: providerKeys.vendors(),
    queryFn: () => listProviderVendors()
  }));

  $effect(() => {
    if (!providerKind && providerKinds.data?.[0])
      providerKind = providerKinds.data[0].kind;
  });

  const discounts = createQuery(() => {
    const cursor = pagination.cursor;
    return {
      queryKey: discountKeys.page(cursor),
      queryFn: () => listDiscounts(cursor)
    };
  });

  function scope(discount: {
    provider_kind: string;
    vendor_id: string | null;
    provider_name?: string | null;
  }): string {
    if (discount.provider_name)
      return `${discount.provider_kind} / ${discount.provider_name}`;
    if (discount.vendor_id)
      return `${discount.provider_kind} / ${discount.vendor_id}`;
    return discount.provider_kind;
  }

  async function addDiscount(event: SubmitEvent) {
    event.preventDefault();
    if (!canEdit || create.busy) return;
    notice = '';
    if (!providerKind || !multiplier.trim()) {
      create.error = 'Provider kind and multiplier are required.';
      return;
    }
    await create.submit(
      {
        provider_kind: providerKind as DiscountDraft['provider_kind'],
        vendor_id: vendorId || null,
        provider_id: providerId || null,
        multiplier: multiplier.trim(),
        effective_at: effectiveAt
          ? new Date(effectiveAt).toISOString()
          : undefined
      },
      (input, requestId) => createDiscount(input, requestId),
      async () => {
        notice = 'Discount published. New usage will price under it.';
        multiplier = '';
        providerId = '';
        await queryClient.invalidateQueries({
          queryKey: discountKeys.all
        });
      },
      { failure: (problem) => errorMessage(problem) }
    );
  }
</script>

<section class="stack" aria-labelledby="discounts-title">
  <div class="section-heading">
    <div>
      <h2 class="sr-only" id="discounts-title">Provider cost discounts</h2>
      <p class="section-description">
        Immutable multipliers on provider cost, newest scope winning. A scope
        ends when a new discount at multiplier 1 supersedes it.
      </p>
    </div>
  </div>
  {#if !canEdit}<ReadOnlyNote
      >Your role can view discounts but not publish them.</ReadOnlyNote
    >{/if}
  {#if create.error}<p class="inline-problem" role="alert">
      {create.error}
    </p>{/if}
  {#if notice}<Callout tone="success">{notice}</Callout>{/if}
  <form class="card discount-form" onsubmit={addDiscount}>
    <div class="form-grid">
      <div class="form-field">
        <label for="discount-kind">Provider kind</label><select
          id="discount-kind"
          bind:value={providerKind}
          disabled={!canEdit ||
            providerKinds.isPending ||
            providerKinds.isError}
          >{#each providerKinds.data ?? [] as option (option.kind)}<option
              value={option.kind}>{option.label}</option
            >{/each}</select
        >
      </div>
      <div class="form-field">
        <label for="discount-vendor">Vendor scope</label><select
          id="discount-vendor"
          bind:value={vendorId}
          disabled={!canEdit}
          ><option value="">All vendors using this connector</option
          >{#each (providerVendors.data ?? []).filter((vendor) => vendor.connector === providerKind) as vendor (vendor.id)}<option
              value={vendor.id}>{vendor.name}</option
            >{/each}</select
        >
      </div>
      <div class="form-field">
        <label for="discount-provider">Provider</label><EntityPicker
          id="discount-provider"
          source={providerOptions}
          bind:value={providerId}
          emptyLabel="Every provider of this kind"
          disabled={!canEdit}
        />
      </div>
      <div class="form-field">
        <label for="discount-multiplier">Multiplier</label><input
          id="discount-multiplier"
          bind:value={multiplier}
          inputmode="decimal"
          placeholder="0.90"
          required
          disabled={!canEdit}
        /><small>Provider cost is multiplied by this; 1 ends the scope.</small>
      </div>
      <div class="form-field">
        <label for="discount-effective">Effective at</label><input
          id="discount-effective"
          bind:value={effectiveAt}
          type="datetime-local"
          aria-describedby="discount-effective-help"
          disabled={!canEdit}
        /><small id="discount-effective-help"
          >Leave blank to apply immediately.</small
        >
      </div>
    </div>
    <button
      class="button button-primary"
      type="submit"
      disabled={!canEdit || create.busy || !providerKind}
      >{create.busy ? 'Publishing…' : 'Publish discount'}</button
    >
  </form>
  {#if discounts.isPending}<div class="loading-state" role="status">
      Loading discounts…
    </div>
  {:else if discounts.isError}<div class="inline-problem" role="alert">
      {errorMessage(discounts.error)}
      <button class="text-button" onclick={() => discounts.refetch()}
        >Try again</button
      >
    </div>
  {:else if (discounts.data?.items ?? []).length === 0}
    <EmptyState title="No discounts yet"
      >Add a discount to reflect negotiated provider pricing in cost reports.</EmptyState
    >
  {:else}
    <ScrollTable label="Discounts">
      <table class="data-table">
        <caption class="sr-only">Discounts, newest first</caption>
        <thead
          ><tr
            ><th scope="col">Scope</th><th scope="col">Multiplier</th><th
              scope="col">Effective at</th
            ><th scope="col">Created</th></tr
          ></thead
        >
        <tbody
          >{#each discounts.data?.items ?? [] as discount (discount.id)}<tr
              ><td>{scope(discount)}</td><td class="mono"
                >{discount.multiplier}</td
              ><td>{formatDate(discount.effective_at)}</td><td
                >{formatDate(discount.created_at)}</td
              ></tr
            >{/each}</tbody
        >
      </table>
    </ScrollTable>
    <CursorPagination
      {...cursorPaginationProps(pagination, discounts.data?.nextCursor)}
      label="Discount pages"
    />
  {/if}
</section>
