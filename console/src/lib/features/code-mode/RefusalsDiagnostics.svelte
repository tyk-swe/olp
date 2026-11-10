<script lang="ts">
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage } from '#lib/management/http.js';
  import {
    emptyCursorHistory,
    cursorPaginationProps,
    resetCursor
  } from '#lib/listing/pagination.js';
  import CursorPagination from '#lib/shell/CursorPagination.svelte';
  import { formatDate } from '#lib/format.js';
  import { listCodeRefusals, type CodeRefusalsQuery } from './api.js';
  import DiagnosticFilterBar, {
    type DiagnosticFilterValues
  } from './DiagnosticFilterBar.svelte';
  import LoadError from './LoadError.svelte';
  import { codeKeys } from './codeKeys';
  import { refusalAdvice } from './presentation';
  import { diagnosticSections } from './sections';

  const section = diagnosticSections.refusals;

  let { projectId }: { projectId: string } = $props();
  const client = useQueryClient();
  let form = $state<DiagnosticFilterValues>({});
  let applied = $state<Omit<CodeRefusalsQuery, 'project_id' | 'cursor'>>({});
  let paging = $state(emptyCursorHistory());
  let error = $state('');

  const filters = $derived<CodeRefusalsQuery>({
    ...applied,
    project_id: projectId,
    cursor: paging.cursor
  });
  const refusals = createQuery(() => ({
    queryKey: codeKeys.collection('refusals', filters),
    queryFn: ({ signal }) => listCodeRefusals(filters, signal)
  }));

  function applyFilters() {
    applied = {
      route_id: form.route?.trim() || undefined,
      api_key_id: form.key?.trim() || undefined,
      end_user_digest: form.endUser?.trim() || undefined
    };
    resetCursor(paging);
  }

  function clearFilters() {
    form = {};
    applied = {};
    resetCursor(paging);
  }

  async function refresh() {
    error = '';
    try {
      await client.invalidateQueries(
        { queryKey: codeKeys.root },
        { throwOnError: true }
      );
    } catch (e) {
      error = `Could not refresh. ${errorMessage(e)}`;
    }
  }
</script>

<section class="diagnostics">
  <div class="actions">
    <h2>{section.title}</h2>
    <button
      class="button button-secondary"
      type="button"
      onclick={refresh}
      disabled={refusals.isFetching}>Refresh</button
    >
  </div>
  <DiagnosticFilterBar
    fields={['route', 'key', 'endUser']}
    bind:values={form}
    onApply={applyFilters}
    onClear={clearFilters}
  />
  {#if error}<p class="field-error" role="alert">{error}</p>{/if}
  {#if refusals.isPending}
    <p role="status">Loading diagnostics…</p>
  {:else if refusals.isError}
    <LoadError error={refusals.error} onRetry={() => refusals.refetch()} />
  {:else if !refusals.data?.items.length}
    <p>No matching {section.empty}.</p>
  {:else}
    {#each refusals.data.items as refusal (refusal.id)}
      <article class="card">
        <h3>{refusal.code}</h3>
        <dl>
          <dt>When</dt>
          <dd>{formatDate(refusal.occurred_at)}</dd>
          <dt>Route / key</dt>
          <dd>{refusal.route_id} / {refusal.api_key_id}</dd>
          <dt>End user</dt>
          <dd>{refusal.end_user_digest || 'Unidentified'}</dd>
          <dt>Refusal ID</dt>
          <dd>{refusal.id}</dd>
        </dl>
        <p>{refusalAdvice(refusal.code)}</p>
      </article>
    {/each}
  {/if}
  <CursorPagination
    {...cursorPaginationProps(paging, refusals.data?.nextCursor)}
    label="Diagnostic pages"
  />
</section>

<style>
  .diagnostics,
  article {
    display: grid;
    gap: 1rem;
  }
  article {
    padding: 1.25rem;
  }
  .actions {
    display: flex;
    align-items: center;
    flex-wrap: wrap;
    gap: 0.75rem;
  }
  h2 {
    flex: 1;
    font-size: 1.25rem;
    font-weight: 600;
  }
  h3 {
    font-weight: 600;
    overflow-wrap: anywhere;
  }
  dl {
    display: grid;
    grid-template-columns: minmax(8rem, 1fr) 3fr;
    gap: 0.6rem 1rem;
  }
  dd,
  p {
    color: var(--foreground-subtle);
    line-height: 1.6;
    overflow-wrap: anywhere;
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
