<script lang="ts">
  import { resolve } from '$app/paths';
  import { createQuery } from '@tanstack/svelte-query';
  import CursorPagination from '$lib/components/CursorPagination.svelte';
  import NavIcon from '$lib/components/NavIcon.svelte';
  import { ApiProblem } from '$lib/api/http';
  import { listRouteDraftPage, listRoutePage } from '$lib/api/management/routes';
  import type { RouteListState } from './routeListState';

  let { listState }: { listState: RouteListState } = $props();

  const drafts = createQuery(() => ({
    queryKey: ['route-draft-page', listState.draftCursor ?? 'first'],
    queryFn: () => listRouteDraftPage(listState.draftCursor)
  }));
  const activeRoutes = createQuery(() => ({
    queryKey: ['route-page', listState.routeCursor ?? 'first'],
    queryFn: () => listRoutePage(listState.routeCursor)
  }));

  function message(error: unknown) {
    return error instanceof ApiProblem
      ? error.problem.detail ?? error.problem.title
      : error instanceof Error ? error.message : 'The control API could not complete the request.';
  }

  function nextDraftPage() {
    const next = drafts.data?.nextCursor;
    if (!next) return;
    listState.draftHistory = [...listState.draftHistory, listState.draftCursor];
    listState.draftCursor = next;
  }

  function previousDraftPage() {
    listState.draftCursor = listState.draftHistory.at(-1);
    listState.draftHistory = listState.draftHistory.slice(0, -1);
  }

  function nextRoutePage() {
    const next = activeRoutes.data?.nextCursor;
    if (!next) return;
    listState.routeHistory = [...listState.routeHistory, listState.routeCursor];
    listState.routeCursor = next;
  }

  function previousRoutePage() {
    listState.routeCursor = listState.routeHistory.at(-1);
    listState.routeHistory = listState.routeHistory.slice(0, -1);
  }
</script>

<svelte:head><title>Routes · OpenLLMProxy</title></svelte:head>

<div class="page-header">
  <div><p class="eyebrow">Gateway</p><h1 class="page-title">Routes</h1><p class="page-description">Stable client-facing slugs backed by explicit, deterministic provider-model targets.</p></div>
  <a class="button button-primary" href={resolve('/routes/new')}>New route draft <NavIcon name="arrow" /></a>
</div>
{#if drafts.isPending || activeRoutes.isPending}
  <div class="loading-state" role="status">Loading routes and drafts…</div>
{:else if drafts.isError || activeRoutes.isError}
  <div class="inline-problem" role="alert">{message(drafts.error ?? activeRoutes.error)} <button class="button button-secondary" type="button" onclick={() => { drafts.refetch(); activeRoutes.refetch(); }}>Retry</button></div>
{:else if !drafts.data?.items.length && !activeRoutes.data?.items.length && listState.draftHistory.length === 0 && listState.routeHistory.length === 0}
  <section class="card empty-state"><div><h2>No routes yet</h2><p>Enable a provider model, then build and simulate a public route slug.</p><a class="button button-primary" href={resolve('/routes/new')}>Build first route</a></div></section>
{:else}
  <section class="route-section" aria-labelledby="active-routes-heading">
    <div class="list-heading"><div><p class="eyebrow">Published runtime</p><h2 id="active-routes-heading">Active routes</h2></div><span class="badge success">{activeRoutes.data?.items.length ?? 0} on this page</span></div>
    {#if !activeRoutes.data?.items.length}
      <div class="card empty-state compact"><p>No active routes on this page.</p></div>
    {:else}
      <div class="table-shell"><table class="data-table"><thead><tr><th>Public slug</th><th>Latest revision</th><th>Operations</th><th>Targets</th><th>Activated</th><th><span class="sr-only">Actions</span></th></tr></thead><tbody>{#each activeRoutes.data.items as item (item.id)}<tr><td><strong><code>{item.slug}</code></strong></td><td>Revision {item.latest_revision.revision}<br /><small>{item.revision_count} total</small></td><td>{item.latest_revision.operations.join(', ')}</td><td>{item.latest_revision.targets.length}</td><td>{new Date(item.latest_revision.activated_at).toLocaleString()}</td><td><a class="button button-secondary" href={resolve(`/routes/${item.id}/revisions`)}>History & restore</a></td></tr>{/each}</tbody></table></div>
    {/if}
    <CursorPagination page={listState.routeHistory.length + 1} hasPrevious={listState.routeHistory.length > 0} hasNext={Boolean(activeRoutes.data?.nextCursor)} onPrevious={previousRoutePage} onNext={nextRoutePage} label="Active route pages" />
  </section>
  <section class="route-section" aria-labelledby="draft-routes-heading">
    <div class="list-heading"><div><p class="eyebrow">Working copies</p><h2 id="draft-routes-heading">Route drafts</h2></div></div>
    {#if !drafts.data?.items.length}
      <div class="card empty-state compact"><p>No unpublished drafts on this page.</p></div>
    {:else}
      <div class="table-shell"><table class="data-table"><thead><tr><th>Slug</th><th>State</th><th>Operations</th><th>Targets</th><th>Deadline / attempts</th><th>Updated</th><th><span class="sr-only">Actions</span></th></tr></thead><tbody>{#each drafts.data.items as item (item.id)}<tr><td><a class="route-link" href={resolve(`/routes/${item.id}`)}>{item.slug}</a></td><td><span class:success={item.state === 'validated'} class:warning={item.state !== 'validated'} class="badge">{item.state}</span></td><td>{item.operations.join(', ')}</td><td>{item.targets.length}</td><td>{item.overall_timeout_ms.toLocaleString()} ms / {item.max_attempts}</td><td>{new Date(item.updated_at).toLocaleString()}</td><td><a class="button button-secondary" href={resolve(`/routes/${item.id}`)}>Open Studio</a></td></tr>{/each}</tbody></table></div>
    {/if}
    <CursorPagination page={listState.draftHistory.length + 1} hasPrevious={listState.draftHistory.length > 0} hasNext={Boolean(drafts.data?.nextCursor)} onPrevious={previousDraftPage} onNext={nextDraftPage} label="Route draft pages" />
  </section>
{/if}

<style>
  h2 { margin: 0 0 .75rem; font-size: 1.15rem; letter-spacing: -.025em; }
  .compact { min-height: 6rem; }
  .route-section { margin-top: 1.5rem; }
  .list-heading { display: flex; min-height: 2.75rem; align-items: center; justify-content: space-between; gap: 1rem; margin-bottom: .6rem; }
  .list-heading h2 { margin: 0; }
  .route-link { color: var(--accent-strong); font-weight: 750; text-underline-offset: .18rem; }
  td small { color: var(--foreground-muted); }
  code { font: .7rem 'JetBrains Mono Variable', monospace; }
</style>
