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
  import { listCodeBudgets, listCodeRoutes, type CodeBudget } from './api.js';
  import BudgetEditor from './BudgetEditor.svelte';
  import LoadError from './LoadError.svelte';
  import { codeKeys } from './codeKeys';
  import { tokenCount } from './presentation';
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

  const section = managementSections.budgets;
  const client = useQueryClient();
  let editing = $state<{ current?: CodeBudget } | null>(null);
  let paging = $state(emptyCursorHistory());
  let error = $state('');
  let notice = $state('');
  const filters = $derived({ project_id: projectId, cursor: paging.cursor });
  const budgets = createQuery(() => ({
    queryKey: codeKeys.collection('budgets', filters),
    queryFn: ({ signal }) => listCodeBudgets(filters, signal)
  }));
  const projectRoutes = createQuery(() => ({
    queryKey: codeKeys.inventory(projectId, 'budgets'),
    queryFn: ({ signal }) =>
      collectCursorPages((cursor) =>
        listCodeRoutes({ project_id: projectId, cursor }, signal)
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

<div class="section">
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
      disabled={budgets.isFetching}>Refresh</button
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
    {#if projectRoutes.isError}
      <LoadError
        error={projectRoutes.error}
        onRetry={() => projectRoutes.refetch()}
      >
        <button
          class="button button-secondary"
          type="button"
          onclick={() => (editing = null)}>Cancel</button
        >
      </LoadError>
    {:else if projectRoutes.data}
      {#key editing}
        <BudgetEditor
          current={editing.current}
          {projectId}
          routes={projectRoutes.data}
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
  {#if budgets.isPending}
    <p role="status">Loading budgets…</p>
  {:else if budgets.isError}
    <LoadError error={budgets.error} onRetry={() => budgets.refetch()} />
  {:else if !budgets.data?.items.length}
    <p class="empty">No {section.empty} in this project.</p>
  {:else}
    <p>
      Daily and monthly periods use UTC. These are token caps, never
      subscription allowance or billed spend. Existing key
      request/rate/concurrency limits also apply. Paid API targets additionally
      require known pricing and monetary admission.
    </p>
    {#each budgets.data.items as budget (budget.id)}
      <article class="card">
        <div class="actions">
          <h3>Token budget</h3>
          {#if allowed}
            <button
              class="button button-secondary"
              type="button"
              disabled={!!editing}
              onclick={() => (editing = { current: budget })}
              >Edit budget</button
            >
          {/if}
        </div>
        <dl>
          <dt>Budget</dt>
          <dd>{budget.id} · {budget.enabled ? 'enabled' : 'disabled'}</dd>
          <dt>Route / key scope</dt>
          <dd>
            {budget.route_id ?? 'All project routes'} / {budget.api_key_id ??
              'All project keys'}
          </dd>
          <dt>Daily tokens</dt>
          <dd>
            {budget.daily_tokens == null
              ? 'No cap'
              : tokenCount(budget.daily_tokens)}
          </dd>
          <dt>Monthly tokens</dt>
          <dd>
            {budget.monthly_tokens == null
              ? 'No cap'
              : tokenCount(budget.monthly_tokens)}
          </dd>
        </dl>
      </article>
    {/each}
  {/if}
  <CursorPagination
    {...cursorPaginationProps(paging, budgets.data?.nextCursor)}
    label="budgets pages"
  />
</div>

<style>
  .section {
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
  dd,
  p {
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
