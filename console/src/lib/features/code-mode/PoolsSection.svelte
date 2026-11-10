<script lang="ts">
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { collectCursorPages } from '#lib/management/pagination.js';
  import { errorMessage } from '#lib/management/http.js';
  import {
    emptyCursorHistory,
    cursorPaginationProps
  } from '#lib/listing/pagination.js';
  import CursorPagination from '#lib/shell/CursorPagination.svelte';
  import ReadOnlyNote from '#lib/shell/ReadOnlyNote.svelte';
  import { listCodeAccounts, listCodePools, type CodePool } from './api.js';
  import LoadError from './LoadError.svelte';
  import PoolEditor from './PoolEditor.svelte';
  import { codeKeys } from './codeKeys';
  import { managementSections } from './sections';

  let {
    projectId,
    allowed,
    onDraftChange
  }: {
    projectId: string;
    allowed: boolean;
    onDraftChange: (dirty: boolean) => void;
  } = $props();

  const section = managementSections.pools;
  const client = useQueryClient();
  let editing = $state<{ current?: CodePool } | null>(null);
  let paging = $state(emptyCursorHistory());
  let error = $state('');
  let notice = $state('');
  const filters = $derived({ project_id: projectId, cursor: paging.cursor });
  const pools = createQuery(() => ({
    queryKey: codeKeys.collection('pools', filters),
    queryFn: ({ signal }) => listCodePools(filters, signal)
  }));
  const projectAccounts = createQuery(() => ({
    queryKey: codeKeys.inventory(projectId, 'pools'),
    queryFn: ({ signal }) =>
      collectCursorPages((cursor) =>
        listCodeAccounts({ project_id: projectId, cursor }, signal)
      ),
    enabled: !!editing
  }));

  async function refresh() {
    error = '';
    notice = '';
    try {
      await client.invalidateQueries(
        { queryKey: codeKeys.root },
        { throwOnError: true }
      );
    } catch (e) {
      error = `Could not refresh. ${errorMessage(e)}`;
    }
  }

  async function onSaved() {
    editing = null;
    onDraftChange(false);
    notice = 'Changes saved.';
    try {
      await client.invalidateQueries(
        { queryKey: codeKeys.root },
        { throwOnError: true }
      );
    } catch (e) {
      error = `Changes saved, but refreshing failed. Use Refresh to reload. ${errorMessage(e)}`;
    }
  }
</script>

<div class="management">
  <div class="actions">
    <h2>{section.title}</h2>
    {#if allowed}
      <button
        class="button button-primary"
        type="button"
        disabled={!!editing}
        onclick={() => (editing = {})}>Create {section.noun}</button
      >
    {/if}
    <button
      class="button button-secondary"
      type="button"
      onclick={refresh}
      disabled={pools.isFetching}>Refresh</button
    >
  </div>
  {#if !allowed}
    <ReadOnlyNote>
      Read only. Changes require configure permission and project manager or
      global authority.
    </ReadOnlyNote>
  {/if}
  {#if error}<p class="field-error" role="alert">{error}</p>{/if}
  {#if notice}<p role="status">{notice}</p>{/if}
  {#if editing}
    {#if projectAccounts.isError}
      <LoadError
        error={projectAccounts.error}
        onRetry={() => projectAccounts.refetch()}
      >
        <button
          class="button button-secondary"
          type="button"
          onclick={() => (editing = null)}>Cancel</button
        >
      </LoadError>
    {:else if projectAccounts.data}
      {#key editing}
        <PoolEditor
          current={editing.current}
          {projectId}
          accounts={projectAccounts.data}
          {allowed}
          {onDraftChange}
          {onSaved}
          onCancel={() => (editing = null)}
        />
      {/key}
    {:else}
      <p role="status">Loading editor choices…</p>
    {/if}
  {/if}
  {#if pools.isPending}
    <p role="status">Loading pools…</p>
  {:else if pools.isError}
    <LoadError error={pools.error} onRetry={() => pools.refetch()} />
  {:else if !pools.data?.items.length}
    <p class="empty">No {section.empty} in this project.</p>
  {:else}
    {#each pools.data.items as pool (pool.id)}
      <article class="card">
        <div class="actions">
          <h3>{pool.name}</h3>
          {#if allowed}
            <button
              class="button button-secondary"
              type="button"
              disabled={!!editing}
              onclick={() => (editing = { current: pool })}>Edit pool</button
            >
          {/if}
        </div>
        <dl>
          <dt>Pool</dt>
          <dd>{pool.id} · {pool.kind}</dd>
          <dt>Owner</dt>
          <dd>{pool.owner_user_id ?? 'Shared project pool'}</dd>
          <dt>Assigned accounts</dt>
          <dd>{pool.account_ids.join(', ') || 'None'}</dd>
          <dt>Explicit key permissions</dt>
          <dd>
            {pool.api_key_ids.join(', ') || 'None — no key can use this pool'}
          </dd>
        </dl>
      </article>
    {/each}
  {/if}
  <CursorPagination
    {...cursorPaginationProps(paging, pools.data?.nextCursor)}
    label="pools pages"
  />
</div>

<style>
  .management {
    display: grid;
    gap: 1rem;
  }
  .actions {
    display: flex;
    gap: 0.65rem;
    align-items: center;
    flex-wrap: wrap;
  }
  h2 {
    flex: 1;
    font-size: 1.25rem;
    font-weight: 600;
  }
  h3 {
    flex: 1;
    font-weight: 600;
  }
  article {
    display: grid;
    gap: 1rem;
    padding: 1.25rem;
  }
  dl {
    display: grid;
    grid-template-columns: minmax(8rem, 1fr) 3fr;
    gap: 0.6rem 1rem;
  }
  dt {
    font-weight: 500;
  }
  dd {
    color: var(--foreground-subtle);
    line-height: 1.6;
    overflow-wrap: anywhere;
  }
  .empty {
    padding: 2rem;
    border: 1px dashed var(--border);
  }
  .field-error {
    color: var(--danger);
  }
  @media (max-width: 40rem) {
    dl {
      grid-template-columns: 1fr;
    }
  }
</style>
