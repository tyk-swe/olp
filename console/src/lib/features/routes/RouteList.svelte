<script lang="ts">
  import Callout from '#lib/components/Callout.svelte';
  import EmptyState from '#lib/components/EmptyState.svelte';
  import PageTitle from '#lib/shell/branding/PageTitle.svelte';
  import QueryView from '#lib/components/QueryView.svelte';
  import ScrollTable from '#lib/components/ScrollTable.svelte';
  import { confirmAction } from '#lib/components/confirm.js';
  import { useMutationIntent } from '#lib/editing/mutationIntent.svelte.js';
  import { useAction } from '#lib/editing/action.svelte.js';

  import { routeKeys } from '#lib/domain/routes/routeKeys.js';
  import { retiringTargets } from '#lib/features/routes/routeTargetModel.js';

  import { resolve } from '$app/paths';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage } from '#lib/management/http.js';
  import CursorPagination from '#lib/components/CursorPagination.svelte';
  import NavIcon from '#lib/components/NavIcon.svelte';
  import ReadOnlyNote from '#lib/components/ReadOnlyNote.svelte';
  import { listRouteDraftPage, retireRoute } from '#lib/features/routes/api.js';
  import { listRoutePage, type ActiveRoute } from '#lib/domain/routes/api.js';
  import { cursorPaginationProps } from '#lib/listing/pagination.js';
  import { useRole } from '#lib/session/useRole.svelte.js';
  import { formatDate, formatInteger } from '#lib/format.js';
  import type { RouteListState } from '#lib/features/routes/routeListState.js';

  const retireRouteIntent = useMutationIntent();

  let { listState = $bindable() }: { listState: RouteListState } = $props();
  const access = useRole();
  const canManage = $derived(access.can('routes.manage'));
  const queryClient = useQueryClient();
  const retireAction = useAction();
  const retiring = $derived(retireAction.key || null);
  let retireError = $state<string | null>(null);

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
    if (
      !(await confirmAction({
        title: `Retire ${item.slug}?`,
        body: 'Clients calling this route get errors until you restore and activate a revision.',
        confirmLabel: 'Retire route',
        danger: true
      }))
    )
      return;
    retireError = null;
    await retireAction.run(
      () =>
        retireRouteIntent.submit(
          [item.id, item.etag] as const,
          (input, requestId) => retireRoute(...input, requestId)
        ),
      async (_, current) => {
        await queryClient.invalidateQueries({ queryKey: routeKeys.lists });
        if (!current()) return;
      },
      {
        key: item.id,
        failure: (error) => `${item.slug}: ${errorMessage(error)}`
      }
    );
    retireError = retireAction.error;
  }
</script>

<PageTitle title="Routes" />

<div class="toolbar">
  <div class="page-actions">
    {#if canManage}
      <a class="button button-primary" href={resolve('routes/new')}>
        New route draft <NavIcon name="arrow" />
      </a>
    {/if}
  </div>
</div>
{#if !canManage}<ReadOnlyNote>
    Your role can view routes but not create, edit, or activate them.
  </ReadOnlyNote>{/if}
{#if drafts.isPending && activeRoutes.isPending}
  <div class="loading-state" role="status">Loading routes and drafts…</div>
{:else if !drafts.data?.items.length && !activeRoutes.data?.items.length && !drafts.isError && !activeRoutes.isError && !drafts.isPending && !activeRoutes.isPending && listState.draft.history.length === 0 && listState.route.history.length === 0}
  <EmptyState title="No routes yet">
    Enable a provider model, then build and simulate a public route slug.
    {#snippet action()}
      {#if canManage}<a
          class="button button-primary"
          href={resolve('routes/new')}
        >
          Build first route
        </a>
      {/if}
    {/snippet}
  </EmptyState>
{:else}
  <section class="route-section" aria-labelledby="published-routes-heading">
    <div class="list-heading">
      <div>
        <p class="eyebrow">Published runtime</p>
        <h2 id="published-routes-heading">Published routes</h2>
      </div>
      <span class="badge success">
        {activeRoutes.data?.items.length ?? 0} on this page
      </span>
    </div>
    <!-- Active routes and drafts come from separate endpoints; one failing must not hide the other. -->
    {#if retireError}
      <div class="retire-error">
        <Callout tone="danger">{retireError}</Callout>
      </div>
    {/if}
    <QueryView
      query={activeRoutes}
      compact
      loading="Loading published routes…"
      failure={errorMessage(activeRoutes.error)}
      updating={activeRoutes.isPlaceholderData}
      emptyWhen={!activeRoutes.data?.items.length}
    >
      {#snippet empty()}
        <EmptyState title="No published routes on this page" />
      {/snippet}
      <ScrollTable
        label="Published routes table"
        busy={activeRoutes.isPlaceholderData}
      >
        <table class="data-table">
          <thead>
            <tr>
              <th>Public slug</th><th>Status</th><th>Latest revision</th><th>
                Operations
              </th><th>Targets</th><th>Activated</th><th>Created by</th><th>
                <span class="sr-only">Actions</span>
              </th>
            </tr>
          </thead><tbody>
            {#each activeRoutes.data?.items ?? [] as item (item.id)}{@const retiringCount =
                retiringTargets(item.latest_revision.targets)}<tr>
                <td>
                  <strong><code>{item.slug}</code></strong><br /><small>
                    {item.project_name ?? 'Installation-wide'}
                  </small>
                </td><td>
                  <span class="badge" class:success={item.state === 'active'}>
                    {item.state}
                  </span>
                </td><td>
                  Revision {item.latest_revision.revision}<br /><small>
                    {item.revision_count} total
                  </small>
                </td><td>{item.latest_revision.operations.join(', ')}</td><td>
                  {item.latest_revision.targets.length}{#if retiringCount}<br
                    /><span class="badge warning">
                      {retiringCount} retiring
                    </span>{/if}
                </td><td>
                  {formatDate(item.latest_revision.activated_at)}
                </td><td>{item.created_by_email ?? 'A removed account'}</td><td
                  class="row-actions"
                >
                  <a
                    class="button button-secondary"
                    href={resolve(`routes/${item.id}/revisions`)}
                  >
                    History & restore
                  </a>

                  {#if canManage && item.state === 'active'}
                    <button
                      class="button button-secondary button-danger"
                      type="button"
                      disabled={retiring !== null}
                      onclick={() => retire(item)}
                    >
                      {retiring === item.id ? 'Retiring…' : 'Retire'}
                    </button>{/if}
                </td>
              </tr>{/each}
          </tbody>
        </table>
      </ScrollTable>
    </QueryView>
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
    <QueryView
      query={drafts}
      compact
      loading="Loading route drafts…"
      failure={errorMessage(drafts.error)}
      updating={drafts.isPlaceholderData}
      emptyWhen={!drafts.data?.items.length}
    >
      {#snippet empty()}
        <EmptyState title="No unpublished drafts on this page" />
      {/snippet}
      <ScrollTable label="Route drafts table" busy={drafts.isPlaceholderData}>
        <table class="data-table">
          <thead>
            <tr>
              <th>Slug</th>
              <th>State</th>
              <th>Operations</th>
              <th>Targets</th>
              <th>Deadline / attempts</th>
              <th>Updated</th>
              <th>Created by</th>
              <th><span class="sr-only">Actions</span></th>
            </tr>
          </thead>

          <tbody>
            {#each drafts.data?.items ?? [] as item (item.id)}
              {@const retiringCount = retiringTargets(item.targets)}

              <tr>
                <td>
                  <a class="table-link" href={resolve(`routes/${item.id}`)}>
                    {item.slug}
                  </a>

                  <br />
                  <small>{item.project_name ?? 'Installation-wide'}</small>
                </td>

                <td>
                  <span
                    class:success={item.state === 'validated'}
                    class:warning={item.state !== 'validated'}
                    class="badge"
                  >
                    {item.state}
                  </span>
                </td><td>{item.operations.join(', ')}</td><td>
                  {item.targets.length}{#if retiringCount}<br /><span
                      class="badge warning"
                    >
                      {retiringCount} retiring
                    </span>{/if}
                </td><td>
                  {formatInteger(item.overall_timeout_ms)} ms / {item.max_attempts}
                </td><td>{formatDate(item.updated_at)}</td><td>
                  {item.created_by_email ?? 'A removed account'}
                </td><td>
                  <a
                    class="button button-secondary"
                    href={resolve(`routes/${item.id}`)}
                  >
                    {canManage ? 'Open Studio' : 'View draft'}
                  </a>
                </td>
              </tr>
            {/each}
          </tbody>
        </table>
      </ScrollTable>
    </QueryView>
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
    font-size: var(--text-body);
    font-weight: 500;
    letter-spacing: -0.02em;
  }
  .retire-error {
    margin-bottom: 0.75rem;
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
  .row-actions {
    display: flex;
    flex-wrap: wrap;
    gap: 0.5rem;
  }
  td small {
    color: var(--foreground-muted);
  }
  code {
    font-family: var(--font-mono);
    font-size: var(--text-caption);
  }
</style>
