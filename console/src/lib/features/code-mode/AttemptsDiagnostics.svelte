<script lang="ts">
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { collectCursorPages } from '#lib/management/pagination.js';
  import { errorMessage } from '#lib/management/http.js';
  import {
    emptyCursorHistory,
    cursorPaginationProps,
    resetCursor
  } from '#lib/listing/pagination.js';
  import CursorPagination from '#lib/shell/CursorPagination.svelte';
  import { formatDate } from '#lib/format.js';
  import {
    listCodeAccounts,
    listCodeAttempts,
    type CodeAttemptsQuery
  } from './api.js';
  import DiagnosticFilterBar, {
    type DiagnosticFilterValues
  } from './DiagnosticFilterBar.svelte';
  import DiagnosticsHeader from './DiagnosticsHeader.svelte';
  import LoadError from './LoadError.svelte';
  import { codeKeys } from './codeKeys';
  import {
    allowanceChargeSummary,
    allowanceEvidenceLabel,
    decisionReasonLabel,
    recoveryReasonLabel,
    refusalAdvice,
    reservationLabel,
    servingHealthLabel,
    skipClassLabel,
    tokenCount,
    usageCertaintyLabel
  } from './presentation';
  import { diagnosticSections } from './sections';

  const section = diagnosticSections.attempts;

  let { projectId }: { projectId: string } = $props();
  const client = useQueryClient();
  let form = $state<DiagnosticFilterValues>({});
  let applied = $state<Omit<CodeAttemptsQuery, 'project_id' | 'cursor'>>({});
  let paging = $state(emptyCursorHistory());
  let error = $state('');

  const filters = $derived<CodeAttemptsQuery>({
    ...applied,
    project_id: projectId,
    cursor: paging.cursor
  });
  const attempts = createQuery(() => ({
    queryKey: codeKeys.collection('attempts', filters),
    queryFn: ({ signal }) => listCodeAttempts(filters, signal)
  }));
  const accounts = createQuery(() => ({
    queryKey: codeKeys.inventory(projectId, 'attempt-eligibility'),
    queryFn: ({ signal }) =>
      collectCursorPages((cursor) =>
        listCodeAccounts({ project_id: projectId, cursor }, signal)
      ),
    enabled:
      attempts.data?.items.some((attempt) => attempt.account_id !== null) ===
      true
  }));

  function applyFilters() {
    applied = {
      attempt_id: form.attempt?.trim() || undefined,
      route_id: form.route?.trim() || undefined,
      api_key_id: form.key?.trim() || undefined,
      account_id: form.account?.trim() || undefined,
      binding_id: form.binding?.trim() || undefined,
      end_user_digest: form.endUser?.trim() || undefined
    };
    resetCursor(paging);
  }

  function clearFilters() {
    form = {};
    applied = {};
    resetCursor(paging);
  }

  function showConversation(bindingId: string) {
    form = { binding: bindingId };
    applied = { binding_id: bindingId };
    paging = emptyCursorHistory();
  }

  function showSource(attemptId: string) {
    form = { attempt: attemptId };
    applied = { attempt_id: attemptId };
    paging = emptyCursorHistory();
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
  <DiagnosticsHeader
    title={section.title}
    fetching={attempts.isFetching}
    onRefresh={refresh}
  />
  <DiagnosticFilterBar
    fields={['attempt', 'route', 'key', 'account', 'binding', 'endUser']}
    bind:values={form}
    onApply={applyFilters}
    onClear={clearFilters}
  />
  {#if error}<p class="field-error" role="alert">{error}</p>{/if}
  {#if attempts.isPending}
    <p role="status">Loading diagnostics…</p>
  {:else if attempts.isError}
    <LoadError error={attempts.error} onRetry={() => attempts.refetch()} />
  {:else if !attempts.data?.items.length}
    <p>
      No matching {section.empty}.
      {#if applied.attempt_id}
        The source may be unavailable under your current project access
      {/if}
    </p>
  {:else}
    <p>
      Measured total includes cached input and reasoning output; those
      breakdowns are subsets. Unknown consumption is never zero. Reservations
      are not billed tokens or provider allowance.
    </p>
    {#each attempts.data.items as attempt (attempt.id)}
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
          <dt>Published Codex CLI identity policy</dt>
          <dd>
            {attempt.codex_cli_identity_enabled ? 'Enabled' : 'Disabled'}. Route
            policy does not prove a header changed or the attempt used Codex.
            Client identity header values and prompts are not recorded.
          </dd>
          <dt>Key / account</dt>
          <dd>
            {attempt.api_key_id} / {attempt.paid_provider_id
              ? `Paid API ${attempt.paid_provider_id} / ${attempt.paid_slot_id}`
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
              Current eligibility does not explain historical selection or grant
              permission to retry. The account decision below records the
              admission-time selection
            </p>
          </dd>
          <dt>Account decision</dt>
          <dd>
            {#if attempt.paid_provider_id}
              Paid target, account selection does not apply
            {:else if attempt.decision}
              {@const decision = attempt.decision}
              {decisionReasonLabel(decision.reason)} · chosen {decision.chosen_account_id}
              {#if decision.evidence}
                · {allowanceEvidenceLabel(decision.evidence)} observed {decision.evidence_observed_at
                  ? formatDate(decision.evidence_observed_at)
                  : 'Unknown'}
              {/if}
              {#if decision.band}· band {decision.band}{/if}
              {#if decision.recovery_envelope}
                · recovery {decision.recovery_envelope} #{decision.recovery_ordinal}
              {/if}
              {#if decision.skipped.length}
                <p>
                  Skipped {decision.skipped
                    .map(
                      (skipped) =>
                        `${skipped.account_id} (${skipClassLabel(skipped.class)})`
                    )
                    .join(', ')}
                </p>
              {/if}
            {:else}
              No decision recorded for this attempt
            {/if}
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
              {@const recoveryFrom = attempt.recovery_from}
              {recoveryReasonLabel(attempt)} ·
              <button
                type="button"
                class="button button-secondary"
                onclick={() => showSource(recoveryFrom)}>{recoveryFrom}</button
              >
            {:else}
              No recovery source recorded
            {/if}
            {#if attempt.paid_binding_id ?? attempt.binding_id}
              {@const conversationBinding = (attempt.paid_binding_id ??
                attempt.binding_id)!}
              <button
                type="button"
                class="button button-secondary"
                onclick={() => showConversation(conversationBinding)}
                >Show conversation attempts</button
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
          <dt>Observed charge</dt>
          <dd>
            {allowanceChargeSummary(attempt)}. A reported charge counts toward
            plan usage limits as reported, and never states remaining allowance
          </dd>
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
          {#if attempt.refusal}
            <dt>Refusal</dt>
            <dd>
              {attempt.refusal}
              <p>{refusalAdvice(attempt.refusal)}</p>
            </dd>
          {/if}
        </dl>
        {#if attempt.state === 'uncertain'}
          <p>
            Consumption is uncertain. The reservation remains charged durably,
            including after disconnect or restart. OLP does not retry or replay
            this request.
          </p>
        {/if}
      </article>
    {/each}
  {/if}
  <CursorPagination
    {...cursorPaginationProps(paging, attempts.data?.nextCursor)}
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
