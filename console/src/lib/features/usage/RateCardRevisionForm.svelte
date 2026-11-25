<script lang="ts">
  import PriceFields from './PriceFields.svelte';
  import { operationKinds } from './history/api';
  import { emptyPriceRow, type PriceEntryRow } from './priceEntry.js';
  import type { ProviderKindCapability } from '#lib/features/providers/index.js';
  import type { ProviderVendor } from '#lib/features/providers/index.js';

  let {
    cardId,
    rows = $bindable(),
    markup = $bindable(),
    effectiveAt = $bindable(),
    canEdit,
    busy,
    kinds,
    vendors,
    onSubmit
  }: {
    cardId: string;
    rows: PriceEntryRow[];
    markup: string;
    effectiveAt: string;
    canEdit: boolean;
    busy: boolean;
    kinds: ProviderKindCapability[];
    vendors: ProviderVendor[];
    onSubmit: (event: SubmitEvent) => void;
  } = $props();
</script>

<form class="revision-form" onsubmit={onSubmit}>
  <div class="form-grid">
    <div class="form-field">
      <label for={`markup-${cardId}`}>Markup</label><input
        id={`markup-${cardId}`}
        bind:value={markup}
        inputmode="decimal"
        placeholder="1.25"
        required
        disabled={!canEdit}
      />
    </div>
    <div class="form-field">
      <label for={`revision-effective-${cardId}`}>Effective at</label><input
        id={`revision-effective-${cardId}`}
        bind:value={effectiveAt}
        type="datetime-local"
        aria-describedby="card-effective-help"
        disabled={!canEdit}
      /><small id="card-effective-help">
        Leave blank to apply immediately.
      </small>
    </div>
  </div>
  {#each rows as row, index (index)}
    <fieldset class="extra-components card">
      <legend>Explicit price {index + 1}</legend>
      <div class="form-grid">
        <div class="form-field">
          <label for={`card-kind-${index}`}>Provider kind</label><select
            id={`card-kind-${index}`}
            bind:value={row.providerKind}
            disabled={!canEdit}
          >
            {#each kinds as option (option.kind)}<option value={option.kind}>
                {option.label}
              </option>{/each}
          </select>
        </div>
        <div class="form-field">
          <label for={`card-vendor-${index}`}>Vendor scope</label><select
            id={`card-vendor-${index}`}
            bind:value={row.vendorId}
            disabled={!canEdit}
          >
            <option value="">
              All vendors
            </option>{#each vendors.filter((vendor) => vendor.connector === row.providerKind) as vendor (vendor.id)}<option
                value={vendor.id}
              >
                {vendor.name}
              </option>{/each}
          </select>
        </div>
        <div class="form-field">
          <label for={`card-model-${index}`}>Model</label><input
            id={`card-model-${index}`}
            bind:value={row.model}
            required
            disabled={!canEdit}
          />
        </div>
        <div class="form-field">
          <label for={`card-operation-${index}`}>Operation</label><select
            id={`card-operation-${index}`}
            bind:value={row.operation}
            disabled={!canEdit}
          >
            {#each operationKinds as option (option)}<option value={option}>
                {option}
              </option>{/each}
          </select>
        </div>
      </div>
      <PriceFields
        idPrefix={`card-${index}`}
        {canEdit}
        bind:inputPrice={row.inputPrice}
        bind:cachedInputPrice={row.cachedInputPrice}
        bind:cacheWritePrice={row.cacheWritePrice}
        bind:cacheWrite5mPrice={row.cacheWrite5mPrice}
        bind:cacheWrite1hPrice={row.cacheWrite1hPrice}
        bind:outputPrice={row.outputPrice}
        bind:unitPrice={row.unitPrice}
        bind:audioInputPrice={row.audioInputPrice}
        bind:cachedAudioInputPrice={row.cachedAudioInputPrice}
        bind:audioOutputPrice={row.audioOutputPrice}
        bind:imageInputPrice={row.imageInputPrice}
        bind:cachedImageInputPrice={row.cachedImageInputPrice}
        bind:imageOutputPrice={row.imageOutputPrice}
        bind:perRequestPrice={row.perRequestPrice}
        bind:trainingPrice={row.trainingPrice}
        bind:storagePrice={row.storagePrice}
        bind:cacheStoragePrice={row.cacheStoragePrice}
        bind:containerFees={row.containerFees}
        bind:batchMultiplier={row.batchMultiplier}
        bind:toolFees={row.toolFees}
        bind:tiers={row.tiers}
        bind:windowZone={row.windowZone}
        bind:windowPeriods={row.windowPeriods}
      />
      <button
        type="button"
        class="text-button"
        onclick={() => {
          rows = rows.filter((_, i) => i !== index);
        }}
      >
        Remove price
      </button>
    </fieldset>
  {/each}
  <button
    type="button"
    class="text-button"
    onclick={() => {
      rows = [...rows, emptyPriceRow(kinds[0]?.kind ?? '')];
    }}
  >
    Add explicit price
  </button>
  <button
    class="button button-primary"
    type="submit"
    disabled={!canEdit || busy}
  >
    {busy ? 'Publishing…' : 'Publish revision'}
  </button>
</form>
