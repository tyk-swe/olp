<script lang="ts">
  import DisclosureSettings from './DisclosureSettings.svelte';
  import { createQuery } from '@tanstack/svelte-query';
  import { catalogKeys, listCatalog, getPublicCatalog } from './api';
  import { errorMessage } from '$lib/api/http';

  let { publicProjectId = '' }: { publicProjectId?: string } = $props();
  const catalog = createQuery(() => ({
    queryKey: publicProjectId
      ? [...catalogKeys.root, 'public', publicProjectId]
      : catalogKeys.root,
    queryFn: ({ signal }) =>
      publicProjectId
        ? getPublicCatalog(publicProjectId, signal)
        : listCatalog(signal)
  }));
  let search = $state('');
  let copied = $state('');
  let copyError = $state('');
  const models = $derived(
    (catalog.data?.items ?? []).filter((model) =>
      model.id.toLowerCase().includes(search.trim().toLowerCase())
    )
  );
  const declaration = (value: boolean | null) =>
    value === null ? 'Unknown' : value ? 'Yes' : 'No';
  const range = (value?: { minimum: string; maximum: string } | null) =>
    value
      ? value.minimum === value.maximum
        ? value.minimum
        : `${value.minimum}–${value.maximum}`
      : 'Unknown';
  async function copy(code: string, label: string) {
    copied = copyError = '';
    try {
      await navigator.clipboard.writeText(code);
      copied = label;
    } catch {
      copyError = 'Copy is unavailable. Select and copy the example text.';
    }
  }
</script>

<svelte:head><title>Model catalog · OpenLLMProxy</title></svelte:head>
<header class="catalog-heading">
  <div>
    <p class="eyebrow">Gateway</p>
    <h1>Model catalog</h1>
    <p class="muted">
      {#if publicProjectId}Published routes shared by this project’s owner, with
        capabilities and SDK examples.{:else}Published routes available to your
        account, with common capabilities and current declared prices.{/if}
    </p>
  </div>
  <button
    class="button button-secondary"
    disabled={catalog.isFetching}
    onclick={() => catalog.refetch()}>Refresh catalog</button
  >
</header>
<label class="catalog-search"
  >Find a route<input
    class="filter-control"
    type="search"
    bind:value={search}
    placeholder="Route name"
  /></label
>
{#if catalog.isPending}<p role="status">Loading the model catalog…</p>
{:else if catalog.isError}<p class="error" role="alert">
    {errorMessage(catalog.error)}
  </p>
{:else if models.length === 0}<p class="muted">
    No published routes match this view.
  </p>
{/if}
{#if copyError}<p class="error" role="alert">{copyError}</p>{/if}
{#if copied}<p role="status">Copied {copied} example.</p>{/if}
{#if !publicProjectId}<DisclosureSettings />{/if}
<div class="catalog-grid">
  {#each models as model (model.id)}
    <article class="catalog-card">
      <h2>{model.id}</h2>
      <p class="muted">{model.capabilities.operations.join(' · ')}</p>
      <dl class="facts">
        <div>
          <dt>Context limit</dt>
          <dd>
            {model.capabilities.context_length?.toLocaleString() ?? 'Unknown'} tokens
          </dd>
        </div>
        <div>
          <dt>Output limit</dt>
          <dd>
            {model.capabilities.max_output_tokens?.toLocaleString() ??
              'Unknown'} tokens
          </dd>
        </div>
        <div>
          <dt>Input modalities</dt>
          <dd>{model.capabilities.input_modalities.join(', ') || 'Unknown'}</dd>
        </div>
        <div>
          <dt>Output modalities</dt>
          <dd>
            {model.capabilities.output_modalities.join(', ') || 'Unknown'}
          </dd>
        </div>
        <div>
          <dt>Data collection declared</dt>
          <dd>{declaration(model.privacy.data_collection)}</dd>
        </div>
        <div>
          <dt>Zero retention declared</dt>
          <dd>{declaration(model.privacy.zero_data_retention)}</dd>
        </div>
        <div>
          <dt>Declared regions</dt>
          <dd>{model.privacy.regions.join(', ') || 'Unknown'}</dd>
        </div>
      </dl>
      {#if model.capabilities.unknown.length || model.privacy.unknown.length}<p
          class="muted"
        >
          Some target facts are unknown. Capabilities reflect the common
          published declarations.
        </p>{/if}
      {#if model.upstream_models?.length}<p>
          <strong>Disclosed upstream models:</strong>
          {model.upstream_models.join(', ')}
        </p>{/if}
      <h3>Prices</h3>
      {#if model.prices?.length}
        {#each model.prices as price (`${price.operation}:${price.currency}`)}
          <p class="price-label">
            {price.operation} · {price.currency}{#if !price.complete}
              · Partial coverage{/if}
          </p>
          <dl class="facts">
            <div>
              <dt>Input / million tokens</dt>
              <dd>{range(price.input_per_million)}</dd>
            </div>
            <div>
              <dt>Cached input / million</dt>
              <dd>{range(price.cached_input_per_million)}</dd>
            </div>
            <div>
              <dt>Output / million tokens</dt>
              <dd>{range(price.output_per_million)}</dd>
            </div>
            {#each [['Cache write / million', price.cache_write_input_per_million], ['Cache write 5 min / million', price.cache_write_5m_input_per_million], ['Cache write 1 hour / million', price.cache_write_1h_input_per_million]] as [label, bounds] (label)}
              {#if bounds}<div>
                  <dt>{label}</dt>
                  <dd>
                    {range(bounds as { minimum: string; maximum: string })}
                  </dd>
                </div>{/if}
            {/each}
            {#if price.unit_price}<div>
                <dt>Per unit</dt>
                <dd>{range(price.unit_price)}</dd>
              </div>{/if}
          </dl>
        {/each}
      {:else}<p class="muted">
          {#if catalog.data?.prices_visible}No current price declaration is
            available.{:else}Prices are not published for this catalog.{/if}
        </p>{/if}
      {#each model.samples as sample (`${sample.sdk}:${sample.operation}`)}
        <details class="sample">
          <summary>{sample.sdk} SDK · {sample.operation}</summary><button
            class="button button-secondary"
            onclick={() => copy(sample.code, `${model.id} ${sample.sdk}`)}
            >Copy {sample.sdk} example</button
          >
          <!-- svelte-ignore a11y_no_noninteractive_tabindex (Keyboard focus enables scrolling the complete example.) -->
          <pre
            role="region"
            aria-label={`${model.id}: ${sample.sdk} ${sample.operation} SDK example`}
            tabindex="0"><code>{sample.code}</code></pre>
        </details>
      {/each}
    </article>
  {/each}
</div>

<style>
  h1 {
    font-size: 1.75rem;
    line-height: 1.2;
    font-weight: 600;
    margin: 0.5rem 0;
  }
  h2 {
    font-size: 1.25rem;
    font-weight: 600;
  }
  h3 {
    font-weight: 600;
    margin-top: 1rem;
  }
  .catalog-heading {
    display: flex;
    align-items: start;
    justify-content: space-between;
    gap: 1.5rem;
    margin-bottom: 1.5rem;
  }
  h1 {
    margin: 0;
  }
  h2,
  h3 {
    margin: 0 0 0.75rem;
    overflow-wrap: anywhere;
  }
  h3 {
    font-size: 1rem;
    margin-top: 1.5rem;
  }
  .muted,
  dt {
    color: var(--foreground-muted);
  }
  .catalog-search {
    display: grid;
    gap: 0.5rem;
    max-width: 28rem;
    margin-bottom: 1.5rem;
  }
  .catalog-grid {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(min(100%, 27rem), 1fr));
    gap: 1rem;
  }
  .catalog-card {
    min-width: 0;
    padding: 1.5rem;
    border: 1px solid var(--border);
    border-radius: var(--radius-card);
    background: var(--surface-raised);
  }
  .facts {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: 1rem;
    margin: 1rem 0;
  }
  .facts div {
    min-width: 0;
  }
  dt {
    font-size: 0.8125rem;
  }
  dd {
    margin: 0.25rem 0 0;
    overflow-wrap: anywhere;
  }
  .price-label {
    font-weight: 600;
  }
  .sample {
    border-top: 1px solid var(--border);
    margin-top: 1rem;
    padding-top: 1rem;
  }
  summary {
    cursor: pointer;
    font-weight: 600;
  }
  .sample button {
    margin-top: 1rem;
  }
  pre {
    max-width: 100%;
    overflow: auto;
    padding: 1rem;
    background: var(--surface);
    border-radius: var(--radius-control);
    font-size: 0.8125rem;
  }
  .error {
    color: var(--danger);
  }
  @media (max-width: 40rem) {
    .catalog-heading {
      flex-direction: column;
    }
    .facts {
      grid-template-columns: 1fr;
    }
  }
</style>
