<script lang="ts">
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import CursorPagination from '$lib/components/CursorPagination.svelte';
  import ReadOnlyNote from '$lib/components/ReadOnlyNote.svelte';
  import {
    cursorPaginationProps,
    emptyCursorHistory
  } from '$lib/lists/pagination';
  import { errorMessage } from '$lib/api/http';
  import { formatDate } from '$lib/format';
  import { useRole } from '$lib/features/access/session/useRole.svelte';
  import { useServiceCapabilities } from '$lib/features/access/session/serviceCapabilities.svelte';
  import { useMutationIntent } from '$lib/forms/mutationIntent.svelte';
  import { listProviderKinds } from '$lib/features/providers/api/models';
  import { listProviderVendors } from '$lib/features/providers/api/providers';
  import { providerKeys } from '$lib/features/providers/providerKeys';
  import {
    createDiscount,
    listDiscounts,
    type DiscountDraft
  } from '$lib/features/usage/api/discounts';
  import { discountKeys } from '$lib/features/usage/discountKeys';

  const createIntent = useMutationIntent();
  const queryClient = useQueryClient();
  const services = useServiceCapabilities();
  const access = useRole();
  const canEdit = $derived(access.can('pricing.update'));

  let providerKind = $state('');
  let vendorId = $state('');
  let providerId = $state('');
  let multiplier = $state('');
  let effectiveAt = $state('');
  let saving = $state(false);
  let error = $state('');
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
    if (!canEdit || saving) return;
    error = notice = '';
    if (!providerKind || !multiplier.trim()) {
      error = 'Provider kind and multiplier are required.';
      return;
    }
    const submission = createIntent.prepare({
      provider_kind: providerKind as DiscountDraft['provider_kind'],
      vendor_id: vendorId || null,
      provider_id: providerId || null,
      multiplier: multiplier.trim(),
      effective_at: effectiveAt
        ? new Date(effectiveAt).toISOString()
        : undefined
    });
    saving = true;
    try {
      await createDiscount(submission.input, submission.requestId);
      createIntent.reset();
      notice = 'Discount published. New usage will price under it.';
      multiplier = '';
      providerId = '';
      await queryClient.invalidateQueries({
        queryKey: ['pricing-discounts']
      });
    } catch (problem) {
      error = errorMessage(problem);
    } finally {
      saving = false;
    }
  }
</script>

<section class="stack" aria-labelledby="discounts-title">
  <div class="section-heading">
    <div>
      <p class="eyebrow">Discounts</p>
      <h2 id="discounts-title">Provider cost discounts</h2>
      <p class="section-description">
        Immutable multipliers on provider cost, newest scope winning. A scope
        ends when a new discount at multiplier 1 supersedes it.
      </p>
    </div>
  </div>
  {#if !canEdit}<ReadOnlyNote
      >Your role can view discounts but not publish them.</ReadOnlyNote
    >{/if}
  {#if error}<p class="inline-problem" role="alert">{error}</p>{/if}
  {#if notice}<p class="inline-notice" role="status">{notice}</p>{/if}
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
        <label for="discount-provider">Provider ID</label><input
          id="discount-provider"
          bind:value={providerId}
          class="mono"
          placeholder="Optional UUID"
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
      disabled={!canEdit || saving || !providerKind}
      >{saving ? 'Publishing…' : 'Publish discount'}</button
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
    <div class="card empty-state" role="status">No discounts published.</div>
  {:else}
    <div class="table-shell" role="region" aria-label="Discounts">
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
    </div>
    <CursorPagination
      {...cursorPaginationProps(pagination, discounts.data?.nextCursor)}
      label="Discount pages"
    />
  {/if}
</section>
