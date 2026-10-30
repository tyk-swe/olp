<script lang="ts">
  import { useMutationIntent } from '$lib/forms/mutationIntent.svelte';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import {
    listCodeBindings,
    listCodeAttempts,
    listCodeAccounts,
    listCodeRefusals,
    listCodeTokenWindows,
    retireCodeBinding,
    type CodeBinding,
    type CodeFilters
  } from '$lib/api/code-mode';
  import { errorMessage } from '$lib/api/http';
  import { collectCursorPages } from '$lib/api/pagination';
  import {
    emptyCursorHistory,
    cursorPaginationProps,
    resetCursor
  } from '$lib/lists/pagination';
  import CursorPagination from '$lib/components/CursorPagination.svelte';
  import LoadError from './LoadError.svelte';
  import { formatDate } from '$lib/format';
  import { codeKeys } from './codeKeys';
  import {
    mutationError,
    refusalAdvice,
    reservationLabel,
    recoveryReasonLabel,
    servingHealthLabel,
    tokenCount,
    usageCertaintyLabel,
    utcWindowStart
  } from './presentation';
  import { diagnosticSections, type CodeDiagnosticsKind } from './sections';

  const retireIntent = useMutationIntent();

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
  const emptyFilterForm = {
    attempt: '',
    route: '',
    key: '',
    account: '',
    binding: '',
    endUser: ''
  };
  let filterForm = $state({ ...emptyFilterForm });
  let applied = $state<CodeFilters>({});
  let paging = $state(emptyCursorHistory());
  let busy = $state('');
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
      ...(kind === 'attempts' &&
      attempts.data?.items.some((attempt) => attempt.account_id !== null)
        ? [accounts.refetch()]
        : [])
    ]);
  }
  async function retire(item: CodeBinding) {
    if (
      !allowed ||
      busy ||
      !confirm(
        `Permanently retire tree ${item.root_id}? Every child and resume will be refused. This tree cannot be reassigned to another account.`
      )
    )
      return;
    busy = item.id;
    error = '';
    notice = '';
    let committed = false;
    try {
      const submission = retireIntent.prepare(item.id);
      await retireCodeBinding(submission.input, submission.requestId);
      retireIntent.reset();
      committed = true;
      notice =
        'Tree permanently retired. Start a fresh independent conversation to use another eligible account.';
      await client.invalidateQueries(
        { queryKey: codeKeys.root },
        { throwOnError: true }
      );
    } catch (e) {
      error = committed
        ? `Binding retired, but refreshing failed. ${errorMessage(e)}`
        : mutationError(e);
    } finally {
      busy = '';
    }
  }
</script>

<section class="diagnostics" aria-label="Code-mode diagnostics">
  <div class="actions">
    <h2>{section.title}</h2>
    <button
      type="button"
      class="button button-secondary"
      disabled={selected.isFetching || accounts.isFetching || !!busy}
      onclick={refresh}>Refresh</button
    >
  </div>
  <p>
    Metadata only: no prompts, outputs, tool payloads, raw headers or
    credentials. Account recovery requires proven nonexecution and a safe
    continuation. Accepted work and uncertain consumption are never replayed
  </p>
  {#if section.filters.route || section.filters.key}
    <form class="filters form-field" onsubmit={apply}>
      {#if kind === 'attempts'}
        <div>
          <label for="code-filter-attempt">Attempt ID</label><input
            id="code-filter-attempt"
            bind:value={filterForm.attempt}
            placeholder="All attempts"
          />
        </div>
      {/if}
      {#if section.filters.route}<div>
          <label for="code-filter-route">Route ID</label><input
            id="code-filter-route"
            bind:value={filterForm.route}
            placeholder="All routes"
          />
        </div>{/if}
      {#if section.filters.key}<div>
          <label for="code-filter-key">API-key ID</label><input
            id="code-filter-key"
            bind:value={filterForm.key}
            placeholder="All keys"
          />
        </div>{/if}
      {#if section.filters.account}<div>
          <label for="code-filter-account">Account ID</label><input
            id="code-filter-account"
            bind:value={filterForm.account}
            placeholder="All accounts"
          />
        </div>{/if}
      {#if section.filters.binding}<div>
          <label for="code-filter-binding">Binding ID</label><input
            id="code-filter-binding"
            bind:value={filterForm.binding}
            placeholder="All bindings"
          />
        </div>{/if}
      {#if section.filters.endUser}
        <div>
          <label for="code-filter-end-user">End-user digest</label>
          <input
            id="code-filter-end-user"
            bind:value={filterForm.endUser}
            placeholder="All end users"
            pattern={'([0-9a-f]{64}|unidentified)'}
            aria-describedby="code-end-user-help"
          />
          <small id="code-end-user-help"
            >Use a lookup digest, or unidentified.</small
          >
        </div>
      {/if}
      <button class="button button-secondary" type="submit"
        >Apply filters</button
      >
      <button
        class="button button-secondary"
        type="button"
        onclick={clearFilters}>Clear filters</button
      >
    </form>
  {/if}
  {#if error}<p class="field-error" role="alert">{error}</p>{/if}
  {#if notice}<p role="status">{notice}</p>{/if}
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
    <p>
      A tree pins an account to each model at its first use and holds at most
      one account of each subscription. Parents, children, resumes, reconnects
      and compaction stay on that model's account. Recovery can change the
      account only after proven nonexecution and safe continuation checks.
      Revoked permissions still refuse access. Retirement is permanent for the
      entire tree
    </p>
    {#each bindings.data?.items ?? [] as item (item.id)}
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
              ? 'Paid API ' + item.paid_provider_id + ' / ' + item.paid_slot_id
              : item.account_id + ' / ' + item.principal}
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
        {#if allowed && !item.retired_at}<button
            type="button"
            class="button button-secondary danger-button"
            disabled={!!busy}
            onclick={() => retire(item)}
            >{busy === item.id ? 'Retiring…' : 'Retire entire tree'}</button
          >{/if}
      </article>
    {/each}
  {:else if kind === 'attempts'}
    <p>
      Measured total includes cached input and reasoning output; those
      breakdowns are subsets. Unknown consumption is never zero. Reservations
      are not billed tokens or provider allowance.
    </p>
    {#each attempts.data?.items ?? [] as attempt (attempt.id)}
      {@const currentAccount = accounts.data?.find(
        (account) => account.id === attempt.account_id
      )}
      <article class="card">
        <h3>{attempt.operation} · {attempt.model}</h3>
        <dl>
          <dt>Requested model</dt>
          <dd>{attempt.requested_model ?? 'Unknown for historical attempt'}</dd>
          <dt>Selected model</dt>
          <dd>{attempt.model}</dd>
          <dt>Attempt / state</dt>
          <dd>{attempt.id} / {attempt.state}</dd>
          <dt>Upstream status</dt>
          <dd>{attempt.upstream_status ?? 'Not observed'}</dd>
          <dt>Outcome origin / result</dt>
          <dd>
            {attempt.outcome_origin ?? 'Not observed'} / {attempt.outcome ??
              'Not observed'}
          </dd>
          <dt>Outcome observed</dt>
          <dd>
            {attempt.outcome_observed_at
              ? formatDate(attempt.outcome_observed_at)
              : 'Not observed'}
          </dd>
          <dt>Route / revision</dt>
          <dd>{attempt.route_id} / {attempt.route_revision_id}</dd>
          <dt>Key / account</dt>
          <dd>
            {attempt.api_key_id} / {attempt.paid_provider_id
              ? 'Paid API ' +
                attempt.paid_provider_id +
                ' / ' +
                attempt.paid_slot_id
              : attempt.account_id}
          </dd>
          <dt>Current account eligibility</dt>
          <dd>
            {#if attempt.paid_provider_id}
              Paid target, subscription eligibility does not apply
            {:else if currentAccount}
              {currentAccount.eligible
                ? 'Currently eligible'
                : 'Currently ineligible'}
              · {currentAccount.enabled
                ? 'Account enabled'
                : 'Account disabled'}
              · {servingHealthLabel(currentAccount.health)} · grant {currentAccount.grant_state}
            {:else}
              {accounts.isPending ? 'Loading current account' : 'Unknown'}
            {/if}
            <p>
              Admission-time eligibility was not retained. Current eligibility
              does not explain historical selection or grant permission to retry
            </p>
          </dd>
          <dt>Recorded recovery evidence</dt>
          <dd>
            {attempt.nonexecution
              ? 'Proven not executed'
              : 'Nonexecution not proven'}
            · {attempt.initial_recovery
              ? 'Portable initial input recorded'
              : 'Portable initial input not recorded'}
          </dd>
          <dt>Recovery source</dt>
          <dd>
            {#if attempt.recovery_from}
              {recoveryReasonLabel(attempt)} ·
              <button
                type="button"
                class="button button-secondary"
                onclick={() => showSource(attempt.recovery_from!)}
                >{attempt.recovery_from}</button
              >
            {:else}
              No recovery source recorded
            {/if}
            {#if attempt.paid_binding_id ?? attempt.binding_id}
              <button
                type="button"
                class="button button-secondary"
                onclick={() =>
                  showConversation(
                    (attempt.paid_binding_id ?? attempt.binding_id)!
                  )}>Show conversation attempts</button
              >
            {/if}
          </dd>
          <dt>End user</dt>
          <dd>{attempt.end_user_digest || 'Unidentified'}</dd>
          {#if Object.keys(attempt.attribution ?? {}).length}
            <dt>Attribution</dt>
            <dd>
              {Object.entries(attempt.attribution ?? {})
                .map(([key, value]) => `${key}=${value}`)
                .join(', ')}
            </dd>
          {/if}
          <dt>Binding</dt>
          <dd>{attempt.paid_binding_id ?? attempt.binding_id}</dd>
          <dt>{reservationLabel(attempt)}</dt>
          <dd>{tokenCount(attempt.reserved_tokens)} tokens</dd>
          <dt>Measured tokens</dt>
          <dd>{tokenCount(attempt.reported_tokens)}</dd>
          <dt>Usage certainty</dt>
          <dd>{usageCertaintyLabel(attempt)}</dd>
          {#if attempt.paid_binding_id}
            <dt>Paid accounting</dt>
            <dd>{attempt.paid_monetary_state ?? 'unknown'}</dd>
          {/if}
          <dt>Input / output</dt>
          <dd>
            {tokenCount(attempt.input_tokens)} / {tokenCount(
              attempt.output_tokens
            )}
          </dd>
          <dt>Cached / reasoning subsets</dt>
          <dd>
            {tokenCount(attempt.cached_tokens)} / {tokenCount(
              attempt.reasoning_tokens
            )}
          </dd>
          <dt>Bound evidence</dt>
          <dd>{attempt.bound_evidence ?? 'No proven bound recorded'}</dd>
          <dt>Started / finished</dt>
          <dd>
            {formatDate(attempt.created_at)} / {attempt.finished_at
              ? formatDate(attempt.finished_at)
              : 'Pending'}
          </dd>
          {#if attempt.refusal}<dt>Refusal</dt>
            <dd>
              {attempt.refusal}
              <p>{refusalAdvice(attempt.refusal)}</p>
            </dd>{/if}
        </dl>
        {#if attempt.state === 'uncertain'}<p>
            Consumption is uncertain. The reservation remains charged durably,
            including after disconnect or restart. OLP does not retry or replay
            this request.
          </p>{/if}
      </article>
    {/each}
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
  .diagnostics,
  article {
    display: grid;
    gap: 1rem;
  }
  article {
    padding: 1.25rem;
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
  .actions,
  .filters {
    display: flex;
    align-items: end;
    flex-wrap: wrap;
    gap: 0.75rem;
  }
  .filters > div {
    display: grid;
    gap: 0.4rem;
    flex: 1 1 12rem;
  }
  label {
    font-size: 0.875rem;
    font-weight: 500;
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
  .danger-button {
    color: var(--danger);
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
