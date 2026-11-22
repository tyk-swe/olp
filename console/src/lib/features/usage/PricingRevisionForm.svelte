<script lang="ts">
  import EntityPicker from '#lib/components/EntityPicker.svelte';
  import { providerOptions } from '#lib/features/providers/index.js';
  import PriceFields from './PriceFields.svelte';
  import { operationKinds } from './history/api';
  import type { ProviderKind } from '#lib/features/providers/index.js';
  import type { ProviderVendor } from '#lib/features/providers/index.js';
  import type { PricingRevisionFormValues } from './priceEntry.js';

  let {
    form,
    canEdit,
    busy,
    limitsEnforced,
    kindsPending,
    kindsError,
    kinds,
    vendors,
    onSubmit
  }: {
    form: PricingRevisionFormValues;
    canEdit: boolean;
    busy: boolean;
    limitsEnforced: boolean;
    kindsPending: boolean;
    kindsError: boolean;
    kinds: { kind: ProviderKind; label: string }[];
    vendors: ProviderVendor[];
    onSubmit: (event: SubmitEvent) => void;
  } = $props();
</script>

<form class="card price-form" onsubmit={onSubmit}>
  <div class="form-grid">
    <div class="form-field">
      <label for="provider-kind">Provider kind</label><select
        id="provider-kind"
        bind:value={form.providerKind}
        onchange={() => {
          form.vendorId = '';
        }}
        disabled={!canEdit || kindsPending || kindsError}
        >{#each kinds ?? [] as option (option.kind)}<option value={option.kind}
            >{option.label}</option
          >{/each}</select
      >{#if kindsPending}<small>Loading provider capabilities…</small
        >{:else if kindsError}<small class="inline-problem"
          >Provider capabilities are unavailable; pricing changes are disabled.</small
        >{/if}
    </div>
    <div class="form-field">
      <label for="price-vendor">Vendor scope</label><select
        id="price-vendor"
        bind:value={form.vendorId}
        disabled={!canEdit}
        ><option value="">All vendors using this connector</option
        >{#each vendors.filter((vendor) => vendor.connector === form.providerKind) as vendor (vendor.id)}<option
            value={vendor.id}>{vendor.name}</option
          >{/each}</select
      >
    </div>
    <div class="form-field">
      <label for="provider-id">Only for provider</label><EntityPicker
        id="provider-id"
        source={providerOptions}
        bind:value={form.providerId}
        emptyLabel="Every provider of this kind"
        disabled={!canEdit}
      />
    </div>
    <div class="form-field">
      <label for="price-model">Upstream model</label><input
        id="price-model"
        bind:value={form.model}
        required
        disabled={!canEdit}
      />
    </div>
    <div class="form-field">
      <label for="price-operation">Operation</label><select
        id="price-operation"
        bind:value={form.operation}
        disabled={!canEdit}
        >{#each operationKinds as option (option)}<option value={option}
            >{option}</option
          >{/each}</select
      >
    </div>
    <PriceFields
      idPrefix="price"
      {canEdit}
      bind:inputPrice={form.inputPrice}
      bind:cachedInputPrice={form.cachedInputPrice}
      bind:cacheWritePrice={form.cacheWritePrice}
      bind:cacheWrite5mPrice={form.cacheWrite5mPrice}
      bind:cacheWrite1hPrice={form.cacheWrite1hPrice}
      bind:outputPrice={form.outputPrice}
      bind:unitPrice={form.unitPrice}
      bind:audioInputPrice={form.audioInputPrice}
      bind:cachedAudioInputPrice={form.cachedAudioInputPrice}
      bind:audioOutputPrice={form.audioOutputPrice}
      bind:imageInputPrice={form.imageInputPrice}
      bind:cachedImageInputPrice={form.cachedImageInputPrice}
      bind:imageOutputPrice={form.imageOutputPrice}
      bind:perRequestPrice={form.perRequestPrice}
      bind:trainingPrice={form.trainingPrice}
      bind:storagePrice={form.storagePrice}
      bind:cacheStoragePrice={form.cacheStoragePrice}
      bind:containerFees={form.containerFees}
      bind:batchMultiplier={form.batchMultiplier}
      bind:toolFees={form.toolFees}
      bind:tiers={form.tiers}
      bind:windowZone={form.windowZone}
      bind:windowPeriods={form.windowPeriods}
    />
    <div class="form-field full">
      <label for="currency">Currency</label><input
        id="currency"
        bind:value={form.currency}
        maxlength="3"
        required
        disabled={!canEdit}
      />
    </div>
    <div class="form-field full">
      <label for="effective-at">Effective at</label><input
        id="effective-at"
        bind:value={form.effectiveAt}
        type="datetime-local"
        required
        disabled={!canEdit}
      />
    </div>
  </div>

  <button
    class="button button-primary"
    type="submit"
    disabled={!canEdit ||
      !limitsEnforced ||
      busy ||
      !form.providerKind ||
      kindsError}>{busy ? 'Creating…' : 'Create pricing revision'}</button
  >
</form>

<style>
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
  @media (max-width: 36rem) {
    .price-form {
      padding: 1rem;
    }
  }
</style>
