<script lang="ts">
  import { confirmAction } from '#lib/components/confirm.js';
  import { useMutationIntent } from '#lib/editing/mutationIntent.svelte.js';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage } from '#lib/management/http.js';
  import {
    emptyCursorHistory,
    cursorPaginationProps,
    resetCursor
  } from '#lib/listing/pagination.js';
  import CursorPagination from '#lib/shell/CursorPagination.svelte';
  import { formatDate } from '#lib/format.js';
  import {
    listCodeBindings,
    retireCodeBinding,
    type CodeBinding,
    type CodeBindingsQuery
  } from './api.js';
  import DiagnosticFilterBar, {
    type DiagnosticFilterValues
  } from './DiagnosticFilterBar.svelte';
  import LoadError from './LoadError.svelte';
  import { codeKeys } from './codeKeys';
  import { mutationError } from './presentation';
  import { diagnosticSections } from './sections';

  const retireIntent = useMutationIntent();
  const section = diagnosticSections.bindings;

  let { projectId, allowed }: { projectId: string; allowed: boolean } =
    $props();
  const client = useQueryClient();
  let form = $state<DiagnosticFilterValues>({});
  let applied = $state<Omit<CodeBindingsQuery, 'project_id' | 'cursor'>>({});
  let paging = $state(emptyCursorHistory());
  let busy = $state('');
  let error = $state('');
  let notice = $state('');

  const filters = $derived<CodeBindingsQuery>({
    ...applied,
    project_id: projectId,
    cursor: paging.cursor
  });
  const bindings = createQuery(() => ({
    queryKey: codeKeys.collection('bindings', filters),
    queryFn: ({ signal }) => listCodeBindings(filters, signal)
  }));

  function applyFilters() {
    applied = {
      route_id: form.route?.trim() || undefined,
      api_key_id: form.key?.trim() || undefined,
      account_id: form.account?.trim() || undefined
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

  async function retire(item: CodeBinding) {
    if (
      busy ||
      !allowed ||
      !(await confirmAction({
        title: `Retire tree ${item.conversation}?`,
        body: 'Every conversation in this parent/child tree will permanently stop serving. A new independent conversation must use a new client conversation identifier.',
        confirmLabel: 'Retire entire tree',
        danger: true
      }))
    )
      return;
    busy = item.id;
    error = '';
    try {
      const submission = retireIntent.prepare(item.id);
      await retireCodeBinding(submission.input, submission.requestId);
      retireIntent.reset();
      notice = `Retired tree ${item.conversation}.`;
      try {
        await client.invalidateQueries(
          { queryKey: codeKeys.root },
          { throwOnError: true }
        );
      } catch (e) {
        error = `Retired tree ${item.conversation}, but refreshing failed. Use Refresh to reload. ${errorMessage(e)}`;
      }
    } catch (e) {
      error = mutationError(e);
    } finally {
      busy = '';
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
      disabled={bindings.isFetching}>Refresh</button
    >
  </div>
  <DiagnosticFilterBar
    fields={['route', 'key', 'account']}
    bind:values={form}
    onApply={applyFilters}
    onClear={clearFilters}
  />
  {#if error}<p class="field-error" role="alert">{error}</p>{/if}
  {#if notice}<p role="status">{notice}</p>{/if}
  {#if bindings.isPending}
    <p role="status">Loading diagnostics…</p>
  {:else if bindings.isError}
    <LoadError error={bindings.error} onRetry={() => bindings.refetch()} />
  {:else if !bindings.data?.items.length}
    <p>No matching {section.empty}.</p>
  {:else}
    <p>
      A tree pins an account to each model at its first use and holds at most
      one account of each subscription. Parents, children, resumes, reconnects
      and compaction stay on that model's account. Recovery can change the
      account only after proven nonexecution and safe continuation checks.
      Revoked permissions still refuse access. Retirement is permanent for the
      entire tree
    </p>
    {#each bindings.data.items as item (item.id)}
      <article class="card">
        <h3>
          {item.retired_at ? 'Retired tree binding' : 'Pinned tree binding'}
        </h3>
        <dl>
          <dt>Conversation</dt>
          <dd>{item.conversation}</dd>
          <dt>Binding / root</dt>
          <dd>{item.id} / {item.root_id}</dd>
          <dt>Parent binding</dt>
          <dd>{item.parent_id ?? 'Root conversation'}</dd>
          <dt>First target</dt>
          <dd>
            {item.paid_provider_id
              ? `Paid API ${item.paid_provider_id} / ${item.paid_slot_id}`
              : `${item.account_id} / ${item.principal}`}
          </dd>
          <dt>Model pins</dt>
          <dd>
            {item.pins
              .map(
                (pin) =>
                  `${pin.model} -> ${pin.paid_provider_id ?? pin.account_id}`
              )
              .join(', ') || 'No model served yet'}
          </dd>
          <dt>Route / key</dt>
          <dd>{item.route_id} / {item.api_key_id}</dd>
          <dt>Created / retired</dt>
          <dd>
            {formatDate(item.created_at)} / {item.retired_at
              ? formatDate(item.retired_at)
              : 'Not retired'}
          </dd>
        </dl>
        {#if allowed && !item.retired_at}
          <button
            type="button"
            class="button button-secondary button-danger"
            disabled={!!busy}
            onclick={() => retire(item)}
            >{busy === item.id ? 'Retiring…' : 'Retire entire tree'}</button
          >
        {/if}
      </article>
    {/each}
  {/if}
  <CursorPagination
    {...cursorPaginationProps(paging, bindings.data?.nextCursor)}
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
