<script lang="ts">
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import CursorPagination from '$lib/components/CursorPagination.svelte';
  import { ApiProblem } from '$lib/api/http';
  import { invalidateProviderSummaries } from './providerCache';
  import {
    getProvider,
    listProviderModelInventoryPage,
    setProviderModel,
    type ProviderModelInventory
  } from '$lib/api/management/providers';

  const queryClient = useQueryClient();
  let modelCursor = $state<string | undefined>();
  let modelHistory = $state<Array<string | undefined>>([]);
  const models = createQuery(() => ({
    queryKey: ['provider-model-inventory-page', modelCursor ?? 'first'],
    queryFn: () => listProviderModelInventoryPage(modelCursor)
  }));
  let search = $state('');
  let surface = $state('all');
  let busyModel = $state('');
  let errorMessage = $state('');

  const inventory = $derived(models.data?.items ?? []);
  const filtered = $derived(
    inventory.filter(({ provider_name, model }) => {
      const needle = search.trim().toLowerCase();
      const matchesText = !needle || `${provider_name} ${model.display_name} ${model.upstream_model}`.toLowerCase().includes(needle);
      const matchesSurface = surface === 'all' || model.capabilities.some((capability) => capability.surface === surface);
      return matchesText && matchesSurface;
    })
  );
  const enabledCount = $derived(
    inventory.filter(({ model }) => model.enabled && model.availability === 'available').length
  );
  const capabilityCount = $derived(inventory.reduce((count, { model }) => count + model.capabilities.length, 0));
  const providerCount = $derived(new Set(inventory.map((entry) => entry.provider_id)).size);

  function message(error: unknown) {
    return error instanceof ApiProblem
      ? error.problem.detail ?? error.problem.title
      : error instanceof Error ? error.message : 'The model inventory could not be loaded.';
  }

  async function toggle(entry: ProviderModelInventory, enabled: boolean) {
    busyModel = entry.model.id;
    errorMessage = '';
    try {
      const provider = await getProvider(entry.provider_id);
      const capabilities = entry.model.capabilities.map(({ operation, surface, mode }) => ({ operation, surface, mode }));
      await setProviderModel(provider, entry.model.id, enabled, capabilities);
      await Promise.all([
        models.refetch(),
        invalidateProviderSummaries(queryClient)
      ]);
    } catch (error) {
      errorMessage = message(error);
    } finally {
      busyModel = '';
    }
  }

  function nextPage() {
    const next = models.data?.nextCursor;
    if (!next) return;
    modelHistory = [...modelHistory, modelCursor];
    modelCursor = next;
  }

  function previousPage() {
    modelCursor = modelHistory.at(-1);
    modelHistory = modelHistory.slice(0, -1);
  }
</script>

<svelte:head><title>Models · OpenLLMProxy</title></svelte:head>

<div class="page-header">
  <div><p class="eyebrow">Gateway</p><h1 class="page-title">Model inventory</h1><p class="page-description">Route eligibility comes from certified provider, model, operation, surface, and mode tuples.</p></div>
  <a class="button button-primary" href="/providers/new">Discover models</a>
</div>

<div class="metric-grid">
  <article class="card metric-card"><p>Models on page</p><strong>{inventory.length}</strong></article>
  <article class="card metric-card"><p>Route eligible on page</p><strong>{enabledCount}</strong></article>
  <article class="card metric-card"><p>Capability tuples on page</p><strong>{capabilityCount}</strong></article>
  <article class="card metric-card"><p>Providers on page</p><strong>{providerCount}</strong></article>
</div>

{#if errorMessage}<div class="inline-problem" role="alert">{errorMessage}</div>{/if}

<div class="toolbar" role="search">
  <label class="search"><span class="sr-only">Search models</span><input class="filter-control" type="search" bind:value={search} placeholder="Search models or providers" /></label>
  <label class="surface"><span>Client surface</span><select class="filter-control" bind:value={surface}><option value="all">All surfaces</option><option value="open_ai">OpenAI</option><option value="anthropic">Anthropic</option><option value="gemini">Gemini</option></select></label>
</div>

{#if models.isPending}
  <div class="loading-state" role="status">Loading certified models…</div>
{:else if models.isError}
  <div class="inline-problem" role="alert">{message(models.error)} <button class="button button-secondary" type="button" onclick={() => models.refetch()}>Retry</button></div>
{:else if inventory.length === 0}
  <section class="card empty-state"><div><h2>No models discovered</h2><p>Run a provider probe and capability review first.</p><a class="button button-primary" href="/providers">Open providers</a></div></section>
{:else if filtered.length === 0}
  <section class="card empty-state"><div><h2>No matching models</h2><p>Clear the search or select another client surface.</p><button class="button button-secondary" type="button" onclick={() => { search = ''; surface = 'all'; }}>Clear filters</button></div></section>
{:else}
  <div class="table-shell"><table class="data-table"><thead><tr><th>Model</th><th>Provider</th><th>Capability tuples / provenance</th><th>Route eligibility</th></tr></thead><tbody>
    {#each filtered as entry (`${entry.provider_id}-${entry.model.id}`)}
      <tr>
        <td><strong>{entry.model.display_name}</strong>{#if entry.model.availability === 'missing'}<span class="badge warning">missing upstream</span>{/if}<br /><code>{entry.model.upstream_model}</code></td>
        <td><a href={`/providers/${entry.provider_id}`}>{entry.provider_name}</a><br /><span class="badge">{entry.provider_kind.replaceAll('_', ' ')}</span></td>
        <td><div class="capabilities">{#each entry.model.capabilities as capability (`${capability.operation}-${capability.surface}-${capability.mode}`)}<span class:success={capability.source === 'certified'} class:accent={capability.source !== 'certified'} class="badge"><strong>{capability.operation}</strong> {capability.surface} · {capability.mode} · {capability.source}</span>{/each}</div></td>
        <td><label class="eligibility"><input type="checkbox" checked={entry.model.enabled} disabled={busyModel === entry.model.id || (entry.model.availability === 'missing' && !entry.model.enabled)} onchange={(event) => toggle(entry, event.currentTarget.checked)} /><span>{entry.model.enabled ? entry.model.availability === 'missing' ? 'Disable before activation' : 'Enabled' : 'Disabled'}</span></label></td>
      </tr>
    {/each}
  </tbody></table></div>
{/if}

{#if !models.isPending && !models.isError}<CursorPagination page={modelHistory.length + 1} hasPrevious={modelHistory.length > 0} hasNext={Boolean(models.data?.nextCursor)} onPrevious={previousPage} onNext={nextPage} label="Model inventory pages" />{/if}

<aside class="policy-note" aria-label="Capability policy"><strong>No silent semantic loss.</strong> Cross-protocol routes reject operations a target cannot faithfully represent; unknown source fields are not treated as certified support.</aside>

<style>
  .toolbar { gap: 1rem; }
  .search { flex: 1; } .search input { width: min(100%, 30rem); }
  .surface { display: flex; min-height: 2.75rem; align-items: center; gap: .6rem; color: var(--foreground-muted); font-size: .78rem; font-weight: 700; }
  code { font: .75rem 'JetBrains Mono Variable', monospace; }
  td a { color: var(--accent-strong); font-weight: 720; text-underline-offset: .18rem; }
  .capabilities { display: flex; max-width: 38rem; flex-wrap: wrap; gap: .35rem; }
  .capabilities .badge { gap: .25rem; font-weight: 600; }
  .eligibility { display: inline-flex; min-height: 2.75rem; align-items: center; gap: .5rem; font-weight: 700; }
  .policy-note { margin-top: 1rem; padding: 1rem; border-left: 3px solid var(--accent); background: var(--accent-soft); color: var(--foreground-muted); }
  .policy-note strong { color: var(--foreground); }
  .empty-state h2 { margin: 0 0 .35rem; }
  .empty-state p { margin: 0 0 1rem; }
  @media (max-width: 42rem) { .toolbar { align-items: stretch; } .search input { width: 100%; } .surface { display: grid; } }
</style>
