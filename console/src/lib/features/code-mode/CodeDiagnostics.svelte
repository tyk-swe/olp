<script lang="ts">
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import {
    listCodeBindings,
    listCodeAttempts,
    listCodeRefusals,
    listCodeTokenWindows,
    retireCodeBinding,
    type CodeBinding,
    type CodeFilters
  } from '$lib/api/code-mode';
  import { errorMessage } from '$lib/api/http';
  import {
    emptyCursorHistory,
    cursorPaginationProps,
    resetCursor
  } from '$lib/lists/pagination';
  import CursorPagination from '$lib/components/CursorPagination.svelte';
  import { formatDate } from '$lib/format';
  import { codeKeys } from './codeKeys';
  import {
    mutationError,
    refusalAdvice,
    reservationLabel,
    tokenCount,
    utcWindowStart
  } from './presentation';

  let {
    projectId,
    kind,
    allowed
  }: {
    projectId: string;
    kind: 'bindings' | 'attempts' | 'refusals' | 'token-windows';
    allowed: boolean;
  } = $props();
  let route = $state(''),
    key = $state(''),
    account = $state(''),
    binding = $state(''),
    endUser = $state('');
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
      ...(kind === 'attempts' || kind === 'refusals'
        ? { end_user_digest: endUser.trim() || undefined }
        : {}),
      route_id: route.trim() || undefined,
      api_key_id: key.trim() || undefined,
      ...(kind !== 'refusals'
        ? { account_id: account.trim() || undefined }
        : {}),
      ...(kind === 'attempts'
        ? { binding_id: binding.trim() || undefined }
        : {})
    };
    resetCursor(paging);
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
    try {
      await retireCodeBinding(item.id);
      notice =
        'Tree permanently retired. Start a fresh independent conversation to use another eligible account.';
      await client.invalidateQueries({ queryKey: codeKeys.root });
    } catch (e) {
      error = mutationError(e);
    } finally {
      busy = '';
    }
  }
</script>

<section class="diagnostics" aria-label="Code-mode diagnostics">
  <div class="actions">
    <h2>
      {kind === 'bindings'
        ? 'Durable conversation trees'
        : kind === 'attempts'
          ? 'Generation attempts'
          : kind === 'refusals'
            ? 'Admission refusals'
            : 'Hard-budget token windows'}
    </h2>
    <button
      type="button"
      class="button button-secondary"
      disabled={selected.isFetching || !!busy}
      onclick={() => selected.refetch()}>Refresh</button
    >
  </div>
  <p>
    Metadata only: no prompts, outputs, tool payloads, raw headers or
    credentials. OLP never automatically switches a pinned tree's account or
    replays inference.
  </p>
  {#if kind !== 'token-windows'}
    <form class="filters form-field" onsubmit={apply}>
      <div>
        <label for="code-filter-route">Route ID</label><input
          id="code-filter-route"
          bind:value={route}
          placeholder="All routes"
        />
      </div>
      <div>
        <label for="code-filter-key">API-key ID</label><input
          id="code-filter-key"
          bind:value={key}
          placeholder="All keys"
        />
      </div>
      {#if kind !== 'refusals'}<div>
          <label for="code-filter-account">Account ID</label><input
            id="code-filter-account"
            bind:value={account}
            placeholder="All accounts"
          />
        </div>{/if}
      {#if kind === 'attempts'}<div>
          <label for="code-filter-binding">Binding ID</label><input
            id="code-filter-binding"
            bind:value={binding}
            placeholder="All bindings"
          />
        </div>{/if}
      {#if kind === 'attempts' || kind === 'refusals'}
        <div>
          <label for="code-filter-end-user">End-user digest</label>
          <input
            id="code-filter-end-user"
            bind:value={endUser}
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
        onclick={() => {
          route = '';
          key = '';
          account = '';
          binding = '';
          endUser = '';
          applied = {};
          resetCursor(paging);
        }}>Clear filters</button
      >
    </form>
  {/if}
  {#if error}<p class="field-error" role="alert">{error}</p>{/if}
  {#if notice}<p role="status">{notice}</p>{/if}
  {#if selected.isPending}<p role="status">Loading diagnostics…</p>
  {:else if selected.isError}<p role="alert">
      {errorMessage(selected.error)}
      <button
        type="button"
        class="text-button"
        onclick={() => selected.refetch()}>Retry</button
      >
    </p>
  {:else if !selected.data?.items.length}<p>
      No matching {kind === 'token-windows' ? 'token windows' : kind}.
    </p>
  {:else if kind === 'bindings'}
    <p>
      A tree pins an account to each model at its first use and holds at most
      one account of each subscription. Parents, children, resumes, reconnects
      and compaction stay on that model's account, which never changes. Revoked
      permissions still refuse access. Retirement is permanent for the entire
      tree.
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
          <dt>First account / principal</dt>
          <dd>{item.account_id} / {item.principal}</dd>
          <dt>Model pins</dt>
          <dd>
            {item.pins
              .map((pin) => `${pin.model} → ${pin.account_id}`)
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
      <article class="card">
        <h3>{attempt.operation} · {attempt.model}</h3>
        <dl>
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
          <dd>{attempt.api_key_id} / {attempt.account_id}</dd>
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
          <dd>{attempt.binding_id}</dd>
          <dt>{reservationLabel(attempt)}</dt>
          <dd>{tokenCount(attempt.reserved_tokens)} tokens</dd>
          <dt>Measured tokens</dt>
          <dd>{tokenCount(attempt.reported_tokens)}</dd>
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
