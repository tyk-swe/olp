<script lang="ts">
  import RevisionDiff from './RevisionDiff.svelte';
  import { useAction } from '#lib/editing/action.svelte.js';
  import { readResult } from '#lib/editing/result.js';
  import PageTitle from '#lib/shell/branding/PageTitle.svelte';
  import { confirmAction } from '#lib/components/confirm.js';
  import PageHeader from '#lib/components/PageHeader.svelte';
  import Tabs from '#lib/components/Tabs.svelte';
  import { routeTabs } from './routeTabs.js';
  import { useMutationIntent } from '#lib/editing/mutationIntent.svelte.js';
  import { routeKeys } from '#lib/domain/routes/routeKeys.js';

  import { goto } from '$app/navigation';
  import { resolve } from '$app/paths';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage as message } from '#lib/management/http.js';
  import {
    diffRouteRevisions,
    listRouteRevisions,
    restoreRouteRevision,
    type RouteRevision,
    type RouteRevisionDiff
  } from '#lib/features/routes/api.js';
  import { useRole } from '#lib/session/useRole.svelte.js';
  import { fidelityLabel } from '#lib/features/routes/routeEditor.js';
  import { formatDate, formatInteger } from '#lib/format.js';

  const restoreIntent = useMutationIntent();

  let { routeId }: { routeId: string } = $props();

  const queryClient = useQueryClient();
  const access = useRole();
  const canManage = $derived(access.can('routes.manage'));
  const revisions = createQuery(() => ({
    queryKey: routeKeys.revisions(routeId),
    queryFn: () => listRouteRevisions(routeId)
  }));

  const restoreAction = useAction();
  let diffBusy = $state(false);
  const busy = $derived(diffBusy ? 'diff' : restoreAction.key);
  let errorMessage = $state('');
  let fromRevision = $state('');
  let toRevision = $state('');
  let revisionDiff = $state<RouteRevisionDiff | null>(null);

  $effect(() => {
    const items = revisions.data ?? [];
    if (items.length && !toRevision) toRevision = items[0].id;
    if (items.length > 1 && !fromRevision) fromRevision = items[1].id;
  });

  async function compareRevisions() {
    if (!fromRevision || !toRevision || fromRevision === toRevision) {
      errorMessage = 'Choose two different revisions to compare.';
      return;
    }
    diffBusy = true;
    errorMessage = '';
    const result = await readResult(() =>
      diffRouteRevisions(routeId, fromRevision, toRevision)
    );
    diffBusy = false;
    if (result.ok) revisionDiff = result.value;
    else errorMessage = message(result.error);
  }

  async function restore(revision: RouteRevision) {
    if (!canManage) return;
    if (
      !(await confirmAction({
        title: `Restore revision ${revision.revision}?`,
        body: 'It becomes a new draft. The live revision keeps serving traffic until you activate the draft.',
        confirmLabel: 'Restore as draft'
      }))
    )
      return;
    errorMessage = '';
    await restoreAction.run(
      () =>
        restoreIntent.submit(
          { routeId, revisionId: revision.id },
          (input, requestId) =>
            restoreRouteRevision(input.routeId, input.revisionId, requestId)
        ),
      async (restored, current) => {
        const refreshed = await readResult(() =>
          queryClient.invalidateQueries(
            { queryKey: routeKeys.lists },
            { throwOnError: true }
          )
        );
        if (!current()) return;
        if (!refreshed.ok)
          errorMessage = `Draft restored, but refreshing failed. ${message(refreshed.error)}`;
        await goto(resolve(`routes/${restored.id}`));
      },
      {
        key: `restore-${revision.id}`,
        failure: (cause) => message(cause)
      }
    );
    errorMessage = restoreAction.error;
  }
</script>

<PageTitle title="Routes" />

<PageHeader
  title={revisions.data?.[0]?.slug ?? 'Route'}
  description="Restoring an old revision opens it as a new draft. Keys and provider credentials are not rolled back."
>
  {#snippet actions()}
    <a class="button button-secondary" href={resolve('routes')}>All routes</a>
  {/snippet}
</PageHeader>
<Tabs label="Route" tabs={routeTabs(routeId)} />
{#if errorMessage}<div class="inline-problem" role="alert">
    {errorMessage}
  </div>{/if}
{#if revisions.isPending}
  <div class="loading-state" role="status">Loading revision history…</div>
{:else if revisions.isError}
  <div class="inline-problem" role="alert">
    {message(revisions.error)}
    <button
      class="button button-secondary"
      type="button"
      onclick={() => revisions.refetch()}
    >
      Retry
    </button>
  </div>
{:else if !revisions.data?.length}
  <section class="card empty-state">
    <div>
      <h2>No activated revisions</h2>
      <p>Activate a validated route draft to create revision 1.</p>
    </div>
  </section>
{:else}
  <section class="card revision-compare" aria-labelledby="compare-heading">
    <div>
      <p class="eyebrow">Revision diff</p>
      <h2 id="compare-heading">Compare configuration</h2>
    </div>
    <label>
      From<select bind:value={fromRevision}>
        {#each revisions.data as revision (revision.id)}<option
            value={revision.id}
          >
            Revision {revision.revision}
          </option>{/each}
      </select>
    </label><label>
      To<select bind:value={toRevision}>
        {#each revisions.data as revision (revision.id)}<option
            value={revision.id}
          >
            Revision {revision.revision}
          </option>{/each}
      </select>
    </label><button
      class="button button-secondary"
      type="button"
      onclick={compareRevisions}
      disabled={busy === 'diff'}
    >
      {busy === 'diff' ? 'Comparing…' : 'Compare'}
    </button>
  </section>
  {#if revisionDiff}<RevisionDiff diff={revisionDiff} />{/if}
  <div class="table-shell revision-table-shell">
    <table class="data-table revision-table">
      <thead>
        <tr>
          <th>Revision</th><th>Activated</th><th>Fidelity</th><th>
            Operations
          </th><th>Deadline / attempts</th><th>Targets</th><th>
            <span class="sr-only">Actions</span>
          </th>
        </tr>
      </thead><tbody>
        {#each revisions.data as revision (revision.id)}<tr
            id={`revision-${revision.id}`}
          >
            <td data-label="Revision">
              <strong>Revision {revision.revision}</strong><br /><code>
                {revision.id}
              </code><br /><small>
                From draft <code>{revision.source_draft_id}</code>
              </small>
              <details>
                <summary>Routing policy</summary>
                <pre>{JSON.stringify(revision.routing_policy, null, 2)}</pre>
              </details>
            </td><td data-label="Activated">
              {formatDate(revision.activated_at)}<br /><small>
                By {revision.activated_by}
              </small>
            </td><td data-label="Fidelity">
              {fidelityLabel(revision.fidelity, revision.passthrough)}
            </td><td data-label="Operations">
              {revision.operations.join(', ')}
            </td><td data-label="Deadline / attempts">
              {formatInteger(revision.overall_timeout_ms)} ms / {revision.max_attempts}
            </td><td data-label="Targets">{revision.targets.length}</td><td
              class="revision-action"
            >
              {#if canManage}<button
                  class="button button-secondary"
                  type="button"
                  onclick={() => restore(revision)}
                  disabled={Boolean(busy)}
                >
                  {busy === `restore-${revision.id}`
                    ? 'Restoring…'
                    : 'Restore as draft'}
                </button>{/if}
            </td>
          </tr>{/each}
      </tbody>
    </table>
  </div>
{/if}

<style>
  pre {
    max-width: 40rem;
    white-space: pre-wrap;
    overflow-wrap: anywhere;
  }
  summary {
    cursor: pointer;
  }
  h2 {
    margin: 0 0 0.75rem;
    font-size: var(--text-body);
    font-weight: 500;
    letter-spacing: -0.02em;
  }
  code {
    font-family: var(--font-mono);
    font-size: var(--text-caption);
  }
  .revision-table small {
    color: var(--foreground-muted);
  }
  .revision-compare {
    display: flex;
    align-items: end;
    gap: 0.75rem;
    margin: 1.5rem 0 1rem;
    padding: clamp(1.1rem, 2.5vw, 1.5rem);
  }
  .revision-compare > div {
    margin-right: auto;
  }
  .revision-compare h2 {
    margin: 0;
  }
  .revision-compare label {
    display: grid;
    gap: 0.3rem;
    color: var(--foreground-subtle);
    font-family: var(--font-mono);
    font-size: var(--text-caption);
    font-weight: 400;
    letter-spacing: -0.02em;
    text-transform: uppercase;
  }
  /* The select sits inside its caption label, so the control typography is
     reset here. */
  .revision-compare select {
    min-height: 2.5rem;
    padding: 0.5rem 0.75rem;
    border: 1px solid var(--border);
    border-radius: var(--radius-control);
    background: transparent;
    color: var(--foreground);
    transition: border-color var(--motion);
    font-family: var(--font-sans);
    font-size: var(--text-body-sm);
    letter-spacing: normal;
    text-transform: none;
  }
  .revision-compare select:hover {
    border-color: var(--border-strong);
  }
  @media (max-width: 48rem) {
    .revision-compare {
      display: grid;
    }
    .revision-table-shell {
      overflow: visible;
      border: 0;
      background: transparent;
    }
    .revision-table,
    .revision-table tbody {
      display: grid;
      gap: 0.75rem;
    }
    .revision-table thead {
      position: absolute;
      width: 1px;
      height: 1px;
      overflow: hidden;
      clip: rect(0, 0, 0, 0);
      white-space: nowrap;
    }
    .revision-table tbody tr {
      display: grid;
      grid-template-columns: minmax(0, 1fr) minmax(0, 1fr);
      gap: 0.75rem;
      padding: 1.25rem;
      border: 1px solid var(--border-hairline);
      border-radius: var(--radius-card);
      background: transparent;
    }
    .revision-table tbody td {
      display: block;
      min-width: 0;
      min-height: 0;
      padding: 0;
      border: 0;
      overflow-wrap: anywhere;
    }
    .revision-table tbody td::before {
      display: block;
      margin-bottom: 0.2rem;
      content: attr(data-label);
      color: var(--foreground-subtle);
      font-family: var(--font-mono);
      font-size: var(--text-caption);
      font-weight: 400;
      letter-spacing: -0.02em;
      text-transform: uppercase;
    }
    .revision-table tbody td:first-child,
    .revision-table tbody .revision-action {
      grid-column: 1 / -1;
    }
    .revision-table tbody .revision-action::before {
      display: none;
    }
    .revision-table tbody .revision-action .button {
      width: 100%;
    }
  }
</style>
