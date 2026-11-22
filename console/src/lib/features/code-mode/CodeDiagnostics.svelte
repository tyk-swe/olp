<script lang="ts">
  import Callout from '#lib/components/Callout.svelte';
  import { confirmAction } from '#lib/components/confirm.js';
  import { useMutation } from '#lib/editing/action.svelte.js';
  import { readResult } from '#lib/editing/result.js';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import {
    listCodeBindings,
    listCodeAttempts,
    listCodeAccounts,
    listCodeDecisions,
    listCodeRefusals,
    listCodeTokenWindows,
    retireCodeBinding,
    type CodeBinding,
    type CodeDecisionsQuery,
    type CodeRefusalsQuery,
    type CodeBindingsQuery,
    type CodeAttemptsQuery,
    type CodeTokenWindowsQuery
  } from '#lib/features/code-mode/api.js';
  import { errorMessage } from '#lib/management/http.js';
  import { collectCursorPages } from '#lib/management/pagination.js';
  import {
    emptyCursorHistory,
    cursorPaginationProps,
    resetCursor
  } from '#lib/listing/pagination.js';
  import CursorPagination from '#lib/shell/CursorPagination.svelte';
  import LoadError from './LoadError.svelte';
  import CodeAttemptList from './CodeAttemptList.svelte';
  import CodeBindingList from './CodeBindingList.svelte';
  import CodeDiagnosticFilters from './CodeDiagnosticFilters.svelte';
  import { formatDate } from '#lib/format.js';
  import { codeKeys } from './codeKeys';
  import {
    mutationError,
    refusalAdvice,
    tokenCount,
    utcWindowStart
  } from './presentation';
  import {
    diagnosticSections,
    emptyFilterForm,
    type CodeDiagnosticsKind
  } from './sections';

  const retireMutation = useMutation();

  let {
    projectId,
    kind,
    allowed
  }: {
    projectId: string;
    kind: CodeDiagnosticsKind;
    allowed: boolean;
  } = $props();
  const section = $derived(diagnosticSections[kind]);
  let filterForm = $state({ ...emptyFilterForm });
  let applied = $state<
    CodeDecisionsQuery &
      CodeRefusalsQuery &
      CodeBindingsQuery &
      CodeAttemptsQuery &
      CodeTokenWindowsQuery
  >({});
  let paging = $state(emptyCursorHistory());

  let error = $state('');
  let notice = $state('');
  const client = useQueryClient();
  const filters = $derived({
    ...applied,
    project_id: projectId,
    cursor: paging.cursor
  });
  const bindings = createQuery(() => ({
    queryKey: codeKeys.collection('bindings', filters),
    queryFn: ({ signal }) => listCodeBindings(filters, signal),
    enabled: kind === 'bindings'
  }));
  const attempts = createQuery(() => ({
    queryKey: codeKeys.collection('attempts', filters),
    queryFn: ({ signal }) => listCodeAttempts(filters, signal),
    enabled: kind === 'attempts'
  }));
  const accounts = createQuery(() => ({
    queryKey: codeKeys.inventory(projectId, 'attempt-eligibility'),
    queryFn: ({ signal }) =>
      collectCursorPages((cursor) =>
        listCodeAccounts({ project_id: projectId, cursor }, signal)
      ),
    enabled:
      kind === 'attempts' &&
      attempts.data?.items.some((attempt) => attempt.account_id !== null) ===
        true
  }));
  const decisionAttemptIds = $derived(
    (attempts.data?.items ?? [])
      .filter((attempt) => !attempt.paid_provider_id)
      .map((attempt) => attempt.id)
  );
  const decisions = createQuery(() => {
    const ids = decisionAttemptIds;
    return {
      queryKey: [
        ...codeKeys.collection('decisions', { project_id: projectId }),
        ids
      ],
      queryFn: ({ signal }) =>
        Promise.all(
          ids.map(async (id) => {
            const page = await readResult(() =>
              listCodeDecisions(
                { project_id: projectId, attempt_id: id },
                signal
              )
            );
            if (!page.ok)
              return { id, status: 'error' as const, error: page.error };
            return {
              id,
              status: 'success' as const,
              decision: page.value.items[0]
            };
          })
        ),
      enabled: kind === 'attempts' && ids.length > 0
    };
  });
  const decisionByAttempt = $derived(
    new Map(
      (decisions.data ?? []).map((result) => [result.id, result] as const)
    )
  );
  const refusals = createQuery(() => ({
    queryKey: codeKeys.collection('refusals', filters),
    queryFn: ({ signal }) => listCodeRefusals(filters, signal),
    enabled: kind === 'refusals'
  }));
  const windows = createQuery(() => ({
    queryKey: codeKeys.collection('token-windows', filters),
    queryFn: ({ signal }) => listCodeTokenWindows(filters, signal),
    enabled: kind === 'token-windows'
  }));
  const selected = $derived(
    {
      bindings,
      attempts,
      refusals,
      'token-windows': windows
    }[kind]
  );

  function apply(event: SubmitEvent) {
    event.preventDefault();
    applied = {
      ...(kind === 'attempts'
        ? { attempt_id: filterForm.attempt.trim() || undefined }
        : {}),
      ...(section.filters.endUser
        ? { end_user_digest: filterForm.endUser.trim() || undefined }
        : {}),
      ...(section.filters.route
        ? { route_id: filterForm.route.trim() || undefined }
        : {}),
      ...(section.filters.key
        ? { api_key_id: filterForm.key.trim() || undefined }
        : {}),
      ...(section.filters.account
        ? { account_id: filterForm.account.trim() || undefined }
        : {}),
      ...(section.filters.binding
        ? { binding_id: filterForm.binding.trim() || undefined }
        : {})
    };
    resetCursor(paging);
  }
  function clearFilters() {
    filterForm = { ...emptyFilterForm };
    applied = {};
    resetCursor(paging);
  }

  function showConversation(bindingId: string) {
    filterForm = { ...emptyFilterForm, binding: bindingId };
    applied = { binding_id: bindingId };
    paging = emptyCursorHistory();
  }

  function showSource(attemptId: string) {
    filterForm = { ...emptyFilterForm, attempt: attemptId };
    applied = { attempt_id: attemptId };
    paging = emptyCursorHistory();
  }

  async function refresh() {
    await Promise.all([
      selected.refetch(),
      ...(kind === 'attempts' && decisionAttemptIds.length
        ? [decisions.refetch()]
        : []),
      ...(kind === 'attempts' &&
      attempts.data?.items.some((attempt) => attempt.account_id !== null)
        ? [accounts.refetch()]
        : [])
    ]);
  }
  async function retire(item: CodeBinding) {
    if (
      !allowed ||
      retireMutation.key ||
      !(await confirmAction({
        title: `Retire tree ${item.root_id}?`,
        body: 'Every child and resume is refused from now on, and the tree can’t be moved to another account. You can’t undo this.',
        confirmLabel: 'Retire tree',
        danger: true
      }))
    )
      return;
    error = '';
    notice = '';
    await retireMutation.submit(
      item.id,
      (input, requestId) => retireCodeBinding(input, requestId),
      async (_, current) => {
        notice =
          'Tree permanently retired. Start a fresh independent conversation to use another eligible account.';
        await client.invalidateQueries(
          { queryKey: codeKeys.root },
          { throwOnError: true }
        );
        if (!current()) return;
      },
      {
        key: item.id,
        failure: (e, stage) =>
          stage === 'accept'
            ? `Binding retired, but refreshing failed. ${errorMessage(e)}`
            : mutationError(e)
      }
    );
    error = retireMutation.error;
  }
</script>

<section class="diagnostics" aria-label="Code-mode diagnostics">
  <div class="actions">
    <h2>{section.title}</h2>
    <button
      type="button"
      class="button button-secondary"
      disabled={selected.isFetching ||
        accounts.isFetching ||
        !!retireMutation.key}
      onclick={refresh}>Refresh</button
    >
  </div>
  <p>
    Metadata only: no prompts, outputs, tool payloads, raw headers or
    credentials. Account recovery requires proven nonexecution and a safe
    continuation. Accepted work and uncertain consumption are never replayed
  </p>
  <CodeDiagnosticFilters
    bind:filterForm
    {section}
    {kind}
    onApply={apply}
    onClear={clearFilters}
  />
  {#if error}<p class="field-error" role="alert">{error}</p>{/if}
  {#if notice}<Callout tone="success">{notice}</Callout>{/if}
  {#if selected.isPending}<p role="status">Loading diagnostics…</p>
  {:else if selected.isError}<LoadError
      error={selected.error}
      onRetry={() => selected.refetch()}
    />
  {:else if !selected.data?.items.length}<p>
      No matching {section.empty}.
      {#if kind === 'attempts' && applied.attempt_id}
        The source may be unavailable under your current project access
      {/if}
    </p>
  {:else if kind === 'bindings'}
    <CodeBindingList
      items={bindings.data?.items ?? []}
      {allowed}
      retiringKey={retireMutation.key}
      onRetire={retire}
    />
  {:else if kind === 'attempts'}
    <CodeAttemptList
      items={attempts.data?.items ?? []}
      accounts={accounts.data ?? []}
      accountsPending={accounts.isPending}
      {decisionByAttempt}
      decisionsPending={decisions.isPending}
      onShowSource={showSource}
      onShowConversation={showConversation}
    />
  {:else if kind === 'refusals'}
    {#each refusals.data?.items ?? [] as refusal (refusal.id)}
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
  {:else}
    <p>
      UTC windows show reservations and measured tokens charged to each budget.
      Reserved includes pending and retained uncertain reservations; inspect
      attempts to distinguish them. Admission also includes matching consumption
      before a budget was enabled, so these counters alone are not remaining
      capacity. Overlapping budgets must not be summed.
    </p>
    {#each windows.data?.items ?? [] as budget (budget.id)}
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
        {:else}<p>No recorded budget windows.</p>{/each}
      </article>
    {/each}
  {/if}
  <CursorPagination
    {...cursorPaginationProps(paging, selected.data?.nextCursor)}
    label="Diagnostic pages"
  />
</section>

<style>
  .diagnostics {
    display: grid;
    gap: 1rem;
  }
  h2 {
    flex: 1;
    font-size: 1.25rem;
    font-weight: 600;
  }
  .actions {
    display: flex;
    align-items: end;
    flex-wrap: wrap;
    gap: 0.75rem;
  }
  p {
    color: var(--foreground-subtle);
    line-height: 1.6;
    overflow-wrap: anywhere;
  }
</style>
