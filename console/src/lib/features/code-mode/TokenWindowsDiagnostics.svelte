<script lang="ts">
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage } from '#lib/management/http.js';
  import {
    emptyCursorHistory,
    cursorPaginationProps
  } from '#lib/listing/pagination.js';
  import CursorPagination from '#lib/shell/CursorPagination.svelte';
  import { listCodeTokenWindows, type CodeTokenWindowsQuery } from './api.js';
  import DiagnosticsHeader from './DiagnosticsHeader.svelte';
  import LoadError from './LoadError.svelte';
  import { codeKeys } from './codeKeys';
  import { tokenCount, utcWindowStart } from './presentation';
  import { diagnosticSections } from './sections';

  const section = diagnosticSections['token-windows'];

  let { projectId }: { projectId: string } = $props();
  const client = useQueryClient();
  let paging = $state(emptyCursorHistory());
  let error = $state('');

  const filters = $derived<CodeTokenWindowsQuery>({
    project_id: projectId,
    cursor: paging.cursor
  });
  const windows = createQuery(() => ({
    queryKey: codeKeys.collection('token-windows', filters),
    queryFn: ({ signal }) => listCodeTokenWindows(filters, signal)
  }));

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
  <DiagnosticsHeader
    title={section.title}
    fetching={windows.isFetching}
    onRefresh={refresh}
  />
  {#if error}<p class="field-error" role="alert">{error}</p>{/if}
  {#if windows.isPending}
    <p role="status">Loading diagnostics…</p>
  {:else if windows.isError}
    <LoadError error={windows.error} onRetry={() => windows.refetch()} />
  {:else if !windows.data?.items.length}
    <p>No matching {section.empty}.</p>
  {:else}
    <p>
      UTC windows show reservations and measured tokens charged to each budget.
      Reserved includes pending and retained uncertain reservations; inspect
      attempts to distinguish them. Admission also includes matching consumption
      before a budget was enabled, so these counters alone are not remaining
      capacity. Overlapping budgets must not be summed.
    </p>
    {#each windows.data.items as budget (budget.id)}
      <article class="card">
        <h3>Budget {budget.id}</h3>
        {#each budget.windows as window (`${window.period}:${window.starts_at}`)}
          <dl>
            <dt>Window</dt>
            <dd>
              {window.period} · {utcWindowStart(window.starts_at)}
            </dd>
            <dt>Pending + uncertain reserved tokens</dt>
            <dd>{tokenCount(window.reserved)}</dd>
            <dt>Measured tokens</dt>
            <dd>{tokenCount(window.measured)}</dd>
          </dl>
        {:else}
          <p>No recorded budget windows.</p>
        {/each}
      </article>
    {/each}
  {/if}
  <CursorPagination
    {...cursorPaginationProps(paging, windows.data?.nextCursor)}
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
