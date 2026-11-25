<script lang="ts">
  import PageTitle from '#lib/shell/branding/PageTitle.svelte';
  import Callout from '#lib/components/Callout.svelte';
  import QueryView from '#lib/components/QueryView.svelte';
  import BulkRouteCreation from './BulkRouteCreation.svelte';
  import ModelInventoryTable from './ModelInventoryTable.svelte';
  import { overviewKeys } from '#lib/features/overview/index.js';
  import { providerKeys } from '#lib/features/providers/providerKeys.js';

  import { resolve } from '$app/paths';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import CursorPagination from '#lib/components/CursorPagination.svelte';
  import ReadOnlyNote from '#lib/components/ReadOnlyNote.svelte';
  import {
    cursorPaginationProps,
    emptyCursorHistory,
    resetCursor
  } from '#lib/listing/pagination.js';
  import { debouncedSearch } from '#lib/listing/search.svelte.js';
  import { getProvider } from '#lib/features/providers/providers.js';
  import {
    listProviderModelInventoryPage,
    setProviderModel,
    type ProviderModelInventory
  } from '#lib/features/providers/models.js';
  import { errorMessage } from '#lib/management/http.js';
  import { useAction } from '#lib/editing/action.svelte.js';
  import { useRole } from '#lib/session/useRole.svelte.js';

  const access = useRole();
  const canManage = $derived(access.can('providers.manage'));
  const queryClient = useQueryClient();
  const pagination = $state(emptyCursorHistory());
  let search = $state('');
  let surface = $state('all');
  let eligibility = $state('all');
  /// The filters the current page request reflects. They move together with
  /// the cursor reset so a request never pairs a new filter with an old page.
  const applied = $state({ search: '', surface: 'all', eligibility: 'all' });

  function applyFilters() {
    if (
      applied.search === search &&
      applied.surface === surface &&
      applied.eligibility === eligibility
    )
      return;
    applied.search = search;
    applied.surface = surface;
    applied.eligibility = eligibility;
    resetCursor(pagination);
  }
  const debounce = debouncedSearch(applyFilters);

  function clearFilters() {
    search = '';
    surface = 'all';
    eligibility = 'all';
    debounce.applyNow();
  }

  const models = createQuery(() => {
    const cursor = pagination.cursor;
    const enabled =
      applied.eligibility === 'all'
        ? undefined
        : applied.eligibility === 'enabled';
    const text = applied.search;
    const surfaceFilter =
      applied.surface === 'all'
        ? undefined
        : (applied.surface as 'openai' | 'anthropic' | 'gemini');
    return {
      queryKey: providerKeys.filteredModelInventory(
        cursor,
        applied.search,
        applied.surface,
        applied.eligibility
      ),
      queryFn: ({ signal }) =>
        listProviderModelInventoryPage(
          cursor,
          enabled,
          signal,
          text,
          surfaceFilter
        ),
      placeholderData: (previous) => previous
    };
  });
  const toggleAction = useAction();
  // Bumped whenever a toggle fails so the eligibility checkboxes are rebuilt
  // from stored state; the browser already flipped them optimistically, and a
  // one-way `checked` binding will not write back an unchanged value.
  let eligibilityVersion = $state(0);
  let notice = $state('');

  const inventory = $derived(models.data?.items ?? []);
  const filtering = $derived(
    Boolean(applied.search) ||
      applied.surface !== 'all' ||
      applied.eligibility !== 'all'
  );
  const enabledCount = $derived(
    inventory.filter(({ model }) => model.enabled).length
  );
  const capabilityCount = $derived(
    inventory.reduce((count, { model }) => count + model.capabilities.length, 0)
  );
  const providerCount = $derived(
    new Set(inventory.map((entry) => entry.provider_id)).size
  );

  function message(error: unknown) {
    return errorMessage(error, 'The model inventory could not be loaded.');
  }

  async function toggle(entry: ProviderModelInventory, enabled: boolean) {
    if (!canManage) return;
    notice = '';
    await toggleAction.run(
      async () => {
        const provider = await getProvider(entry.provider_id);
        const capabilities = entry.model.capabilities.map(
          ({ operation, surface, mode }) => ({ operation, surface, mode })
        );
        return setProviderModel(
          provider,
          entry.model.id,
          enabled,
          capabilities
        );
      },
      async () => {
        await Promise.all([
          queryClient.invalidateQueries({
            queryKey: providerKeys.summaries
          }),
          queryClient.invalidateQueries({
            queryKey: providerKeys.modelCatalog
          }),
          queryClient.invalidateQueries({ queryKey: overviewKeys.root })
        ]);
        notice = `Model eligibility staged. Activate ${entry.provider_name} to apply the change.`;
      },
      {
        key: entry.model.id,
        failure: (error) => {
          eligibilityVersion += 1;
          void queryClient.invalidateQueries({
            queryKey: providerKeys.modelCatalog
          });
          return message(error);
        }
      }
    );
  }
</script>

<PageTitle title="Models" />

<div class="toolbar">
  <div class="page-actions">
    {#if canManage}<a
        class="button button-primary"
        href={resolve('providers/new')}
      >
        Discover models
      </a>
    {/if}
  </div>
</div>
{#if !canManage}<ReadOnlyNote>
    Your role can view the model inventory but not change route eligibility.
  </ReadOnlyNote>{/if}

<div class="metric-grid">
  <article class="card metric-card">
    <p>Models on page</p>
    <strong>{inventory.length}</strong>
  </article>
  <article class="card metric-card">
    <p>Route eligible on page</p>
    <strong>{enabledCount}</strong>
  </article>
  <article class="card metric-card">
    <p>Capability tuples on page</p>
    <strong>{capabilityCount}</strong>
  </article>
  <article class="card metric-card">
    <p>Providers on page</p>
    <strong>{providerCount}</strong>
  </article>
</div>

{#if toggleAction.error}
  <div class="inline-problem" role="alert">{toggleAction.error}</div>
{/if}

{#if notice}<Callout tone="success">{notice}</Callout>{/if}

<div class="toolbar" role="search">
  <label class="search">
    <span class="sr-only">Search models</span><input
      class="filter-control"
      type="search"
      bind:value={search}
      oninput={() => {
        if (search === '') debounce.applyNow();
        else debounce.schedule();
      }}
      placeholder="Search models or providers"
    />
  </label>
  <label class="filter-choice">
    <span>Client surface</span><select
      class="filter-control"
      bind:value={surface}
      onchange={debounce.applyNow}
    >
      <option value="all">All surfaces</option><option value="openai">
        OpenAI
      </option><option value="anthropic">Anthropic</option><option
        value="gemini"
      >
        Gemini
      </option>
    </select>
  </label>
  <label class="filter-choice">
    <span>Route eligibility</span><select
      class="filter-control"
      bind:value={eligibility}
      onchange={debounce.applyNow}
    >
      <option value="all">Any eligibility</option><option value="enabled">
        Enabled
      </option><option value="disabled">Disabled</option>
    </select>
  </label>
</div>

<BulkRouteCreation
  models={inventory}
  {canManage}
  outdated={models.isPlaceholderData}
/>

<QueryView
  query={models}
  loading="Loading certified models…"
  failure={message(models.error)}
  emptyWhen={!inventory.length}
>
  {#snippet empty()}
    {#if !filtering}
      <section class="card empty-state">
        <div>
          <h2>No models discovered</h2>
          <p>Run a provider probe and capability review first.</p>

          <a class="button button-primary" href={resolve('providers')}>
            Open providers
          </a>
        </div>
      </section>
    {:else}
      <section class="card empty-state">
        <div>
          <h2>No matching models</h2>
          <p>
            Clear the filters, or use the pagination below to search another
            page.
          </p>
          <button
            class="button button-secondary"
            type="button"
            onclick={clearFilters}
          >
            Clear filters
          </button>
        </div>
      </section>
    {/if}
  {/snippet}
  {#if models.isPlaceholderData}
    <p class="updating" role="status">Updating…</p>
  {/if}
  <ModelInventoryTable
    {inventory}
    {canManage}
    busy={toggleAction.busy}
    updating={models.isPlaceholderData}
    {eligibilityVersion}
    onToggle={toggle}
  />
</QueryView>

{#if !models.isPending && !models.isError}<CursorPagination
    {...cursorPaginationProps(
      pagination,
      models.isPlaceholderData ? null : models.data?.nextCursor
    )}
    label="Model pages"
  />{/if}

<aside class="policy-note" aria-label="Capability policy">
  <strong>No silent semantic loss.</strong> Cross-protocol routes reject operations
  a target cannot faithfully represent; unknown source fields are not treated as certified
  support.
</aside>

<style>
  .toolbar {
    gap: 1rem;
  }
  .search {
    flex: 1;
  }
  .search input {
    width: min(100%, 30rem);
  }
  .filter-choice {
    display: flex;
    min-height: 2.75rem;
    align-items: center;
    gap: 0.6rem;
    color: var(--foreground-muted);
    font-size: var(--text-body-sm);
    font-weight: 500;
  }
  .policy-note {
    margin-top: 1rem;
    padding: 1rem;
    border: 0;
    border-radius: var(--radius-control);
    background: var(--surface-raised);
    color: var(--foreground-muted);
  }
  .policy-note strong {
    color: var(--foreground);
  }
  .empty-state h2 {
    margin: 0 0 0.35rem;
  }
  .empty-state p {
    margin: 0 0 1rem;
  }
  @media (max-width: 42rem) {
    .toolbar {
      align-items: stretch;
    }
    .search input {
      width: 100%;
    }
    .filter-choice {
      display: grid;
    }
  }
</style>
