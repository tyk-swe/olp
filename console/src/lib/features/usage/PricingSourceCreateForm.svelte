<script lang="ts">
  import { bundledCatalogHint } from './pricingSourcePresentation.js';
  import type { ReferenceCatalog } from './pricingSources.js';
  import type { PricingSourceFormat } from './pricingSources.js';

  let {
    name = $bindable(),
    format = $bindable(),
    url = $bindable(),
    ready,
    busy,
    catalog,
    onSubmit
  }: {
    name: string;
    format: PricingSourceFormat;
    url: string;
    ready: boolean;
    busy: boolean;
    catalog: ReferenceCatalog | undefined;
    onSubmit: (event: SubmitEvent) => void;
  } = $props();
  const formatLabels: Record<PricingSourceFormat, string> = {
    prices: 'Price list (JSON)',
    catalog: 'Signed reference catalog'
  };
</script>

<form class="card source-form" onsubmit={onSubmit}>
  <div class="form-grid">
    <div class="form-field">
      <label for="source-name">Source name</label><input
        id="source-name"
        bind:value={name}
        required
      />
    </div>
    <div class="form-field">
      <label for="source-format">Format</label><select
        id="source-format"
        bind:value={format}
      >
        {#each Object.entries(formatLabels) as [value, label] (value)}
          <option {value}>{label}</option>
        {/each}
      </select>
    </div>
    <div class="form-field full">
      <label for="source-url">
        Document URL{format === 'catalog' ? ' (optional)' : ''}
      </label><input
        id="source-url"
        type="url"
        aria-describedby="source-url-help"
        bind:value={url}
        required={format === 'prices'}
        placeholder={format === 'catalog'
          ? 'https://raw.githubusercontent.com/tyk-swe/olp/main/internal/catalog/catalog.json'
          : 'https://prices.example.com/openai.json'}
      /><small id="source-url-help">
        {format === 'catalog'
          ? bundledCatalogHint(catalog)
          : 'A JSON price list, fetched through the egress policy.'}
      </small>
    </div>
  </div>
  <button class="button button-primary" type="submit" disabled={busy || !ready}>
    {busy ? 'Creating…' : 'Create pricing source'}
  </button>
</form>

<style>
  .source-form {
    display: grid;
    gap: 1rem;
    padding: 1.25rem;
  }
  .full {
    grid-column: 1 / -1;
  }
</style>
