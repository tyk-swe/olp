<script lang="ts">
  import { routeKeys } from '$lib/features/routes/routeKeys';

  import { resolve } from '$app/paths';
  import { goto } from '$app/navigation';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage as message } from '$lib/api/http';
  import CursorPagination from '$lib/components/CursorPagination.svelte';
  import NavIcon from '$lib/components/NavIcon.svelte';
  import ReadOnlyNote from '$lib/components/ReadOnlyNote.svelte';
  import {
    createRouteMigrationDraft,
    listRouteDraftPage,
    listRoutePage,
    retireRoute,
    type ActiveRoute
  } from '$lib/features/routes/api';
  import { cursorPaginationProps } from '$lib/lists/pagination';
  import { useRole } from '$lib/features/access/session/useRole.svelte';
  import { formatDate, formatInteger } from '$lib/format';
  import type { RouteListState } from '$lib/features/routes/routeListState';

  let { listState = $bindable() }: { listState: RouteListState } = $props();
  const access = useRole();
  const canManage = $derived(access.can('routes.manage'));
  const queryClient = useQueryClient();
  let retiring = $state<string | null>(null);
  let retireError = $state<string | null>(null);
  let migrationRoute = $state<ActiveRoute | null>(null);
  let migrationSlug = $state('');
  let migrationBusy = $state(false);
  let migrationError = $state<string | null>(null);

  const drafts = createQuery(() => {
    const cursor = listState.draft.cursor;
    return {
      queryKey: routeKeys.draftPage(cursor),
      queryFn: ({ signal }) => listRouteDraftPage(cursor, signal),
      placeholderData: (previous) => previous
    };
  });
  const activeRoutes = createQuery(() => {
    const cursor = listState.route.cursor;
    return {
      queryKey: routeKeys.page(cursor),
      queryFn: ({ signal }) => listRoutePage(cursor, signal),
      placeholderData: (previous) => previous
    };
  });

  async function retire(item: ActiveRoute) {
    if (retiring !== null) return;
    retiring = item.id;
    retireError = null;
    try {
      await retireRoute(item.id, item.etag);
      await queryClient.invalidateQueries({ queryKey: routeKeys.lists });
    } catch (error) {
      retireError = `${item.slug}: ${message(error)}`;
    } finally {
      retiring = null;
    }
  }

  function reviewMigration(item: ActiveRoute) {
    migrationRoute = item;
    // The review draft opens in the route editor, so propose a slug it can
    // save: at most 63 lowercase letters or digits with single hyphens.
    const base = item.slug
      .replace(/[^a-z0-9]+/g, '-')
      .slice(0, 56)
      .replace(/^-|-$/g, '');
    migrationSlug = `${base}-strict`;
    migrationError = null;
  }

  async function createMigration() {
    if (!migrationRoute || migrationBusy) return;
    migrationBusy = true;
    migrationError = null;
    try {
      const draft = await createRouteMigrationDraft(
        migrationRoute,
        migrationSlug
      );
      await queryClient.invalidateQueries({ queryKey: routeKeys.lists });
      await goto(resolve(`/routes/${draft.id}`));
    } catch (error) {
      migrationError = message(error);
    } finally {
      migrationBusy = false;
    }
  }
</script>

<svelte:head><title>Routes · OpenLLMProxy</title></svelte:head>

<div class="page-header">
  <div>
    <p class="eyebrow">Gateway</p>
    <h1 class="page-title">Routes</h1>
    <p class="page-description">
      Publish stable model names for your clients and choose the providers
      behind each route.
    </p>
  </div>
  {#if canManage}<a class="button button-primary" href={resolve('/routes/new')}
      >New route draft <NavIcon name="arrow" /></a
    >{/if}
</div>
{#if !canManage}<ReadOnlyNote
    >Your role can view routes but not create, edit, or activate them.</ReadOnlyNote
  >{/if}
{#if drafts.isPending && activeRoutes.isPending}
  <div class="loading-state" role="status">Loading routes and drafts…</div>
{:else if !drafts.data?.items.length && !activeRoutes.data?.items.length && !drafts.isError && !activeRoutes.isError && !drafts.isPending && !activeRoutes.isPending && listState.draft.history.length === 0 && listState.route.history.length === 0}
  <section class="card empty-state">
    <div>
      <h2>No routes yet</h2>
      <p>
        Enable a provider model, then build and simulate a public route slug.
      </p>
      {#if canManage}<a
          class="button button-primary"
          href={resolve('/routes/new')}>Build first route</a
        >{/if}
    </div>
  </section>
{:else}
  <section class="route-section" aria-labelledby="published-routes-heading">
    <div class="list-heading">
      <div>
        <p class="eyebrow">Published runtime</p>
        <h2 id="published-routes-heading">Published routes</h2>
      </div>
      <span class="badge success"
        >{activeRoutes.data?.items.length ?? 0} on this page</span
      >
    </div>
    <!-- Active routes and drafts come from separate endpoints; one failing must not hide the other. -->
    {#if retireError}
      <div class="inline-problem" role="alert">{retireError}</div>
    {/if}
    {#if activeRoutes.isPending}
      <div class="loading-state" role="status">Loading published routes…</div>
    {:else if activeRoutes.isError}
      <div class="inline-problem" role="alert">
        {message(activeRoutes.error)}
        <button
          class="button button-secondary"
          type="button"
          onclick={() => activeRoutes.refetch()}>Retry</button
        >
      </div>
    {:else if !activeRoutes.data?.items.length}
      <div class="card empty-state compact">
        <p>No published routes on this page.</p>
      </div>
    {:else}
      {#if activeRoutes.isPlaceholderData}
        <p class="updating" role="status">Updating…</p>
      {/if}
      <!-- svelte-ignore a11y_no_noninteractive_tabindex -->
      <div
        class="table-shell"
        tabindex="0"
        role="region"
        aria-label="Published routes table"
        aria-busy={activeRoutes.isPlaceholderData}
      >
        <table class="data-table">
          <thead
            ><tr
              ><th>Public slug</th><th>Status</th><th>Latest revision</th><th
                >Operations</th
              ><th>Targets</th><th>Activated</th><th>Created by</th><th
                ><span class="sr-only">Actions</span></th
              ></tr
            ></thead
          ><tbody
            >{#each activeRoutes.data.items as item (item.id)}<tr
                ><td
                  ><strong><code>{item.slug}</code></strong><br /><small
                    >{item.project_name ?? 'Installation-wide'}</small
                  ></td
                ><td
                  ><span class="badge" class:success={item.state === 'active'}
                    >{item.state}</span
                  ></td
                ><td
                  >Revision {item.latest_revision.revision}<br /><small
                    >{item.revision_count} total</small
                  ></td
                ><td>{item.latest_revision.operations.join(', ')}</td><td
                  >{item.latest_revision.targets.length}</td
                ><td>{formatDate(item.latest_revision.activated_at)}</td><td
                  >{item.created_by_email ?? 'A removed account'}</td
                ><td class="row-actions"
                  ><a
                    class="button button-secondary"
                    href={resolve(`/routes/${item.id}/revisions`)}
                    >History & restore</a
                  >{#if canManage && item.latest_revision.fidelity?.mode !== 'strict'}<button
                      class="button button-secondary"
                      type="button"
                      disabled={migrationBusy}
                      onclick={() => reviewMigration(item)}
                      >Create strict migration draft</button
                    >{/if}
                  >{#if canManage && item.state === 'active'}<button
                      class="button button-secondary"
                      type="button"
                      disabled={retiring !== null}
                      onclick={() => retire(item)}
                      >{retiring === item.id ? 'Retiring…' : 'Retire'}</button
                    >{/if}</td
                ></tr
              >{/each}</tbody
          >
        </table>
      </div>
    {/if}
    {#if migrationRoute}
      <form
        class="card migration-form"
        onsubmit={(event) => {
          event.preventDefault();
          void createMigration();
        }}
      >
        <h3>Review a strict route for {migrationRoute.slug}</h3>
        <p>
          The new slug lets clients move deliberately. This creates an editable
          draft and does not activate a route or call a provider.
        </p>
        <div class="form-field">
          <label for="migration-slug">New route slug</label>
          <input
            id="migration-slug"
            type="text"
            required
            maxlength="63"
            pattern="[a-z0-9]+(-[a-z0-9]+)*"
            bind:value={migrationSlug}
            disabled={migrationBusy}
          />
        </div>
        {#if migrationError}<p class="inline-problem" role="alert">
            {migrationError}
          </p>{/if}
        <div class="row-actions">
          <button
            class="button button-primary"
            type="submit"
            disabled={migrationBusy}
          >
            {migrationBusy ? 'Creating…' : 'Create review draft'}
          </button>
          <button
            class="button button-secondary"
            type="button"
            disabled={migrationBusy}
            onclick={() => (migrationRoute = null)}>Cancel</button
          >
        </div>
      </form>
    {/if}
    {#if !activeRoutes.isError}<CursorPagination
        {...cursorPaginationProps(
          listState.route,
          activeRoutes.isPlaceholderData ? null : activeRoutes.data?.nextCursor
        )}
        label="Published route pages"
      />{/if}
  </section>
  <section class="route-section" aria-labelledby="draft-routes-heading">
    <div class="list-heading">
      <div>
        <p class="eyebrow">Working copies</p>
        <h2 id="draft-routes-heading">Route drafts</h2>
      </div>
    </div>
    {#if drafts.isPending}
      <div class="loading-state" role="status">Loading route drafts…</div>
    {:else if drafts.isError}
      <div class="inline-problem" role="alert">
        {message(drafts.error)}
        <button
          class="button button-secondary"
          type="button"
          onclick={() => drafts.refetch()}>Retry</button
        >
      </div>
    {:else if !drafts.data?.items.length}
      <div class="card empty-state compact">
        <p>No unpublished drafts on this page.</p>
      </div>
    {:else}
      {#if drafts.isPlaceholderData}
        <p class="updating" role="status">Updating…</p>
      {/if}
      <!-- svelte-ignore a11y_no_noninteractive_tabindex -->
      <div
        class="table-shell"
        tabindex="0"
        role="region"
        aria-label="Route drafts table"
        aria-busy={drafts.isPlaceholderData}
      >
        <table class="data-table">
          <thead
            ><tr
              ><th>Slug</th><th>State</th><th>Operations</th><th>Targets</th><th
                >Deadline / attempts</th
              ><th>Updated</th><th>Created by</th><th
                ><span class="sr-only">Actions</span></th
              ></tr
            ></thead
          ><tbody
            >{#each drafts.data.items as item (item.id)}<tr
                ><td
                  ><a class="route-link" href={resolve(`/routes/${item.id}`)}
                    >{item.slug}</a
                  ><br /><small
                    >{item.project_name ?? 'Installation-wide'}</small
                  ></td
                ><td
                  ><span
                    class:success={item.state === 'validated'}
                    class:warning={item.state !== 'validated'}
                    class="badge">{item.state}</span
                  ></td
                ><td>{item.operations.join(', ')}</td><td
                  >{item.targets.length}</td
                ><td
                  >{formatInteger(item.overall_timeout_ms)} ms / {item.max_attempts}</td
                ><td>{formatDate(item.updated_at)}</td><td
                  >{item.created_by_email ?? 'A removed account'}</td
                ><td
                  ><a
                    class="button button-secondary"
                    href={resolve(`/routes/${item.id}`)}
                    >{canManage ? 'Open Studio' : 'View draft'}</a
                  ></td
                ></tr
              >{/each}</tbody
          >
        </table>
      </div>
    {/if}
    {#if !drafts.isError}<CursorPagination
        {...cursorPaginationProps(
          listState.draft,
          drafts.isPlaceholderData ? null : drafts.data?.nextCursor
        )}
        label="Route draft pages"
      />{/if}
  </section>
{/if}

<style>
  h2 {
    margin: 0 0 0.75rem;
    font-size: 1rem;
    font-weight: 500;
    letter-spacing: -0.02em;
  }
  .compact {
    min-height: 6rem;
  }
  .updating {
    margin: 0 0 0.5rem;
    color: var(--foreground-muted);
    font-size: var(--text-body-sm);
  }
  .route-section {
    margin-top: 1.5rem;
  }
  .list-heading {
    display: flex;
    min-height: 2.75rem;
    align-items: center;
    justify-content: space-between;
    gap: 1rem;
    margin-bottom: 0.6rem;
  }
  .list-heading h2 {
    margin: 0;
  }
  .route-link {
    color: var(--foreground);
    font-weight: 400;
    text-decoration: underline;
    text-decoration-color: var(--border-strong);
    text-underline-offset: 4px;
    transition: text-decoration-color var(--motion);
  }
  .route-link:hover {
    text-decoration-color: currentColor;
  }
  .row-actions {
    display: flex;
    flex-wrap: wrap;
    gap: 0.5rem;
  }
  .migration-form {
    margin-top: 1rem;
    padding: 1rem;
  }
  .migration-form p {
    color: var(--foreground-muted);
  }
  td small {
    color: var(--foreground-muted);
  }
  code {
    font-family: var(--font-mono);
    font-size: var(--text-caption);
  }
</style>
