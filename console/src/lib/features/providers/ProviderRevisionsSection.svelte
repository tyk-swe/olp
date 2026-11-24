<script lang="ts">
  import { confirmAction } from '#lib/components/confirm.js';
  import { providerKeys } from '#lib/features/providers/providerKeys.js';

  import { createQuery } from '@tanstack/svelte-query';
  import { errorMessage } from '#lib/management/http.js';
  import {
    cursorPaginationProps,
    emptyCursorHistory
  } from '#lib/listing/pagination.js';
  import CursorPagination from '#lib/components/CursorPagination.svelte';
  import {
    listProviderRevisionPage,
    type ProviderRevisionDiff
  } from '#lib/features/providers/revisions.js';
  import { type Provider } from '#lib/features/providers/providers.js';
  import { formatDate } from '#lib/format.js';
  import type { ProviderEditor } from './providerEditor.svelte';

  let { current, editor }: { current: Provider; editor: ProviderEditor } =
    $props();
  const editorView = $derived(editor.view);
  const busy = $derived(editorView.busy);
  const canManage = $derived(editor.canManage);
  const refreshPending = $derived(editorView.refreshPending);
  const pagination = $state(emptyCursorHistory());
  let viewed = $state<{ id: string; revision: number } | null>(null);
  let revisionFrom = $state('');
  let revisionTo = $state('');
  let revisionDiff = $state<ProviderRevisionDiff | null>(null);
  let comparisonError = $state('');
  const revisions = createQuery(() => ({
    queryKey: providerKeys.revisions(current.id, pagination.cursor),
    queryFn: ({ signal }) =>
      listProviderRevisionPage(current.id, pagination.cursor, signal)
  }));

  $effect(() => {
    const items = revisions.data?.items ?? [];
    if (items.length && !revisionTo) revisionTo = items[0].id;
    if (items.length > 1 && !revisionFrom) revisionFrom = items[1].id;
  });

  async function compare() {
    if (!revisionFrom || !revisionTo || revisionFrom === revisionTo) {
      comparisonError = 'Choose two different revisions to compare.';
      return;
    }
    comparisonError = '';
    const result = await editor.compareRevisions(revisionFrom, revisionTo);
    if (result) revisionDiff = result;
  }

  async function restore(revisionId: string, revision: number) {
    if (!canManage || refreshPending) return;
    if (
      !(await confirmAction({
        title: `Restore revision ${revision}?`,
        body: 'It becomes a new draft. The current credential stays selected and nothing changes until you activate it.',
        confirmLabel: 'Restore as draft'
      }))
    )
      return;
    if (await editor.restoreRevision(revisionId, revision)) revisionDiff = null;
  }

  function resetComparison() {
    revisionFrom = revisionTo = '';
    revisionDiff = null;
  }

  function view(revisionId: string, revision: number) {
    viewed = { id: revisionId, revision };
  }
  import ProviderRevisionComparison from '#lib/features/providers/ProviderRevisionComparison.svelte';
  import ProviderRevisionDetail from './ProviderRevisionDetail.svelte';
</script>

<section
  class="card editor revisions"
  aria-labelledby="provider-revisions-heading"
>
  {#if refreshPending}<p class="inline-problem" role="status">
      The draft was restored. Refresh the view before another restore.
      <button
        type="button"
        class="button button-secondary"
        disabled={Boolean(busy)}
        onclick={editor.retryRefresh}>Retry refresh</button
      >
    </p>{/if}
  <div class="section-heading">
    <div>
      <p class="eyebrow">Immutable history</p>
      <h2 id="provider-revisions-heading">Provider revisions</h2>
      <p class="muted">
        Historical secrets and credential IDs are never returned. Restoring
        copies only non-secret configuration into a new draft and preserves the
        current credential selection.
      </p>
    </div>
  </div>
  {#if revisions.isPending}<p class="inline-status" role="status">
      Loading provider revisions…
    </p>
  {:else if revisions.isError}<div class="inline-problem" role="alert">
      {errorMessage(revisions.error)}
      <button
        class="button button-secondary"
        type="button"
        onclick={() => revisions.refetch()}>Retry</button
      >
    </div>
  {:else if !revisions.data?.items.length && pagination.history.length === 0}<div
      class="empty-state"
    >
      <p>No activated revision exists yet.</p>
    </div>
  {:else}
    {#if (revisions.data?.items.length ?? 0) > 1}<div
        class="revision-compare"
        aria-label="Compare provider revisions"
      >
        <label
          >From<select bind:value={revisionFrom}
            >{#each revisions.data?.items ?? [] as item (item.id)}<option
                value={item.id}>Revision {item.revision}</option
              >{/each}</select
          ></label
        ><label
          >To<select bind:value={revisionTo}
            >{#each revisions.data?.items ?? [] as item (item.id)}<option
                value={item.id}>Revision {item.revision}</option
              >{/each}</select
          ></label
        ><button
          class="button button-secondary"
          type="button"
          onclick={compare}
          disabled={Boolean(busy)}
          >{busy === 'revision-diff' ? 'Comparing…' : 'Compare'}</button
        >
      </div>{/if}
    {#if comparisonError}<p class="inline-problem" role="alert">
        {comparisonError}
      </p>{/if}
    <ProviderRevisionComparison {revisionDiff} />
    <div class="revision-list">
      {#each revisions.data?.items ?? [] as item (item.id)}<article
          class="revision-row"
        >
          <div>
            <strong>Revision {item.revision}</strong><small
              >Activated {formatDate(item.activated_at)} by
              <code>{item.activated_by}</code></small
            ><small
              >{item.model_count} models · credential metadata {item.historical_credential_version ==
              null
                ? 'workload identity'
                : `version ${item.historical_credential_version}`}</small
            >
          </div>
          <div class="revision-actions">
            <button
              class="button button-secondary view-button"
              type="button"
              aria-label={`View revision ${item.revision}`}
              onclick={() => view(item.id, item.revision)}>View</button
            >
            {#if canManage}<button
                class="button button-secondary"
                type="button"
                onclick={() => restore(item.id, item.revision)}
                disabled={Boolean(busy) || refreshPending}
                >{busy === 'revision-restore'
                  ? 'Restoring…'
                  : 'Restore as draft'}</button
              >{/if}
          </div>
        </article>{/each}
    </div>
    <CursorPagination
      {...cursorPaginationProps(
        pagination,
        revisions.data?.nextCursor,
        resetComparison
      )}
      label="Provider revision pages"
    />
  {/if}
</section>

{#if viewed}
  <ProviderRevisionDetail
    providerId={current.id}
    {viewed}
    onClose={() => (viewed = null)}
  />
{/if}

<style>
  .editor {
    margin-top: 1.25rem;
    padding: 1.5rem;
  }
  h2 {
    margin: 0 0 0.75rem;
    font-size: var(--text-body);
    font-weight: 500;
    letter-spacing: -0.02em;
  }
  .revisions {
    max-width: none;
  }
  .section-heading {
    display: flex;
    align-items: flex-start;
    justify-content: space-between;
    gap: 1rem;
  }
  .revision-compare {
    display: flex;
    flex-wrap: wrap;
    align-items: end;
    gap: 0.65rem;
    margin: 1rem 0;
  }
  .revision-compare label {
    display: grid;
    gap: 0.3rem;
    color: var(--foreground-muted);
    font-size: var(--text-body-sm);
    font-weight: 500;
  }
  .revision-compare select {
    min-height: 2.5rem;
    padding: 0.5rem 0.75rem;
    border: 1px solid var(--border);
    border-radius: var(--radius-control);
    background: transparent;
    color: var(--foreground);
    transition: border-color var(--motion);
  }
  .revision-compare select:hover {
    border-color: var(--border-strong);
  }

  .revision-list {
    display: grid;
    gap: 0.6rem;
  }
  .revision-row {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 1rem;
    padding: 0.85rem;
    border: 0;
    border-radius: var(--radius-control);
    background: var(--surface-raised);
  }
  .revision-row > div {
    display: grid;
    min-width: 0;
    gap: 0.2rem;
  }
  .revision-actions {
    display: flex;
    flex-wrap: wrap;
    gap: 0.5rem;
  }
  /* Viewing a revision is the incidental action next to Restore, so it reads
     as a quiet control rather than a second bordered button. */
  .revision-actions .view-button {
    border-color: transparent;
    background: none;
    color: var(--foreground-muted);
  }
  .revision-actions .view-button:hover:not(:disabled) {
    border-color: transparent;
    background: none;
    color: var(--foreground-hover);
  }
  .revision-row small {
    color: var(--foreground-muted);
    overflow-wrap: anywhere;
  }
  code {
    font-size: var(--text-caption);
  }

  @media (max-width: 42rem) {
    .revision-row {
      display: grid;
    }
  }
</style>
