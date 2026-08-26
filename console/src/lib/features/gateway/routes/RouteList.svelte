<script lang="ts">
  import { resolve } from '$app/paths';
  import { createQuery } from '@tanstack/svelte-query';
  import { errorMessage as message } from '$lib/api/http';
  import CursorPagination from '$lib/components/CursorPagination.svelte';
  import NavIcon from '$lib/components/NavIcon.svelte';
  import ReadOnlyNote from '$lib/components/ReadOnlyNote.svelte';
  import { listRouteDraftPage, listRoutePage } from '$lib/api/management/routes';
  import { cursorPaginationProps } from '$lib/api/pagination';
  import { useRole } from '$lib/auth/useRole.svelte';
  import { formatDate } from '$lib/format';
  import type { RouteListState } from './routeListState';

  let { listState = $bindable() }: { listState: RouteListState } = $props();
  const access = useRole();
  const canManage = $derived(access.can('routes.manage'));

  const drafts = createQuery(() => ({
    queryKey: ['route-draft-page', listState.draft.cursor ?? 'first'],
    queryFn: () => listRouteDraftPage(listState.draft.cursor)
  }));
  const activeRoutes = createQuery(() => ({
    queryKey: ['route-page', listState.route.cursor ?? 'first'],
    queryFn: () => listRoutePage(listState.route.cursor)
  }));

</script>

<svelte:head><title>Routes · OpenLLMProxy</title></svelte:head>

<div class="page-header">
  <div><p class="eyebrow">Gateway</p><h1 class="page-title">Routes</h1><p class="page-description">Stable client-facing slugs backed by explicit, deterministic provider-model targets.</p></div>
  {#if canManage}<a class="button button-primary" href={resolve('/routes/new')}>New route draft <NavIcon name="arrow" /></a>{/if}
</div>
{#if !canManage}<ReadOnlyNote>Your role can view routes but not create, edit, or activate them.</ReadOnlyNote>{/if}
{#if drafts.isPending && activeRoutes.isPending}
  <div class="loading-state" role="status">Loading routes and drafts…</div>
{:else if !drafts.data?.items.length && !activeRoutes.data?.items.length && !drafts.isError && !activeRoutes.isError && listState.draft.history.length === 0 && listState.route.history.length === 0}
  <section class="card empty-state"><div><h2>No routes yet</h2><p>Enable a provider model, then build and simulate a public route slug.</p>{#if canManage}<a class="button button-primary" href={resolve('/routes/new')}>Build first route</a>{/if}</div></section>
{:else}
  <section class="route-section" aria-labelledby="active-routes-heading">
    <div class="list-heading"><div><p class="eyebrow">Published runtime</p><h2 id="active-routes-heading">Active routes</h2></div><span class="badge success">{activeRoutes.data?.items.length ?? 0} on this page</span></div>
    <!-- Active routes and drafts come from separate endpoints; one failing must not hide the other. -->
    {#if activeRoutes.isPending}
      <div class="loading-state" role="status">Loading active routes…</div>
    {:else if activeRoutes.isError}
      <div class="inline-problem" role="alert">{message(activeRoutes.error)} <button class="button button-secondary" type="button" onclick={() => activeRoutes.refetch()}>Retry</button></div>
    {:else if !activeRoutes.data?.items.length}
      <div class="card empty-state compact"><p>No active routes on this page.</p></div>
    {:else}
      <div class="table-shell"><table class="data-table"><thead><tr><th>Public slug</th><th>Latest revision</th><th>Operations</th><th>Targets</th><th>Activated</th><th>Created by</th><th><span class="sr-only">Actions</span></th></tr></thead><tbody>{#each activeRoutes.data.items as item (item.id)}<tr><td><strong><code>{item.slug}</code></strong></td><td>Revision {item.latest_revision.revision}<br /><small>{item.revision_count} total</small></td><td>{item.latest_revision.operations.join(', ')}</td><td>{item.latest_revision.targets.length}</td><td>{formatDate(item.latest_revision.activated_at)}</td><td>{item.created_by_email ?? 'A removed account'}</td><td><a class="button button-secondary" href={resolve(`/routes/${item.id}/revisions`)}>History & restore</a></td></tr>{/each}</tbody></table></div>
    {/if}
    {#if !activeRoutes.isError}<CursorPagination {...cursorPaginationProps(listState.route, activeRoutes.data?.nextCursor)} label="Active route pages" />{/if}
  </section>
  <section class="route-section" aria-labelledby="draft-routes-heading">
    <div class="list-heading"><div><p class="eyebrow">Working copies</p><h2 id="draft-routes-heading">Route drafts</h2></div></div>
    {#if drafts.isPending}
      <div class="loading-state" role="status">Loading route drafts…</div>
    {:else if drafts.isError}
      <div class="inline-problem" role="alert">{message(drafts.error)} <button class="button button-secondary" type="button" onclick={() => drafts.refetch()}>Retry</button></div>
    {:else if !drafts.data?.items.length}
      <div class="card empty-state compact"><p>No unpublished drafts on this page.</p></div>
    {:else}
      <div class="table-shell"><table class="data-table"><thead><tr><th>Slug</th><th>State</th><th>Operations</th><th>Targets</th><th>Deadline / attempts</th><th>Updated</th><th>Created by</th><th><span class="sr-only">Actions</span></th></tr></thead><tbody>{#each drafts.data.items as item (item.id)}<tr><td><a class="route-link" href={resolve(`/routes/${item.id}`)}>{item.slug}</a></td><td><span class:success={item.state === 'validated'} class:warning={item.state !== 'validated'} class="badge">{item.state}</span></td><td>{item.operations.join(', ')}</td><td>{item.targets.length}</td><td>{item.overall_timeout_ms.toLocaleString()} ms / {item.max_attempts}</td><td>{formatDate(item.updated_at)}</td><td>{item.created_by_email ?? 'A removed account'}</td><td><a class="button button-secondary" href={resolve(`/routes/${item.id}`)}>{canManage ? 'Open Studio' : 'View draft'}</a></td></tr>{/each}</tbody></table></div>
    {/if}
    {#if !drafts.isError}<CursorPagination {...cursorPaginationProps(listState.draft, drafts.data?.nextCursor)} label="Route draft pages" />{/if}
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
