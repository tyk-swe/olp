<script lang="ts">
  import { errorMessage } from '#lib/management/http.js';
  import { formatDate } from '#lib/format.js';
  import type {
    CodeAttempt,
    CodeAccount,
    CodeDecision
  } from '#lib/features/code-mode/api.js';
  import {
    allowanceChargeSummary,
    allowanceEvidenceLabel,
    decisionReasonLabel,
    refusalAdvice,
    reservationLabel,
    recoveryReasonLabel,
    servingHealthLabel,
    skipClassLabel,
    tokenCount,
    usageCertaintyLabel
  } from './presentation';

  type AttemptDecision =
    | { id: string; status: 'error'; error: unknown }
    | { id: string; status: 'success'; decision: CodeDecision | undefined };

  let {
    items,
    accounts,
    accountsPending,
    decisionByAttempt,
    decisionsPending,
    onShowSource,
    onShowConversation
  }: {
    items: CodeAttempt[];
    accounts: CodeAccount[];
    accountsPending: boolean;
    decisionByAttempt: Map<string, AttemptDecision>;
    decisionsPending: boolean;
    onShowSource: (attemptId: string) => void;
    onShowConversation: (bindingId: string) => void;
  } = $props();
</script>

<p>
  Measured total includes cached input and reasoning output; those breakdowns
  are subsets. Unknown consumption is never zero. Reservations are not billed
  tokens or provider allowance.
</p>
{#each items as attempt (attempt.id)}
  {@const currentAccount = accounts.find(
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
        policy does not prove a header changed or the attempt used Codex. Client
        identity header values and prompts are not recorded.
      </dd>
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
          · {currentAccount.enabled ? 'Account enabled' : 'Account disabled'}
          · {servingHealthLabel(currentAccount.health)} · grant {currentAccount.grant_state}
        {:else}
          {accountsPending ? 'Loading current account' : 'Unknown'}
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
        {:else}
          {@const result = decisionByAttempt.get(attempt.id)}
          {#if result?.status === 'success' && result.decision}
            {@const decision = result.decision}
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
          {:else if result?.status === 'error'}
            <span role="alert"
              >Could not load account decision. {errorMessage(
                result.error
              )}</span
            >
          {:else}
            {decisionsPending
              ? 'Loading account decision'
              : 'No decision recorded for this attempt'}
          {/if}
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
          {recoveryReasonLabel(attempt)} ·
          <button
            type="button"
            class="button button-secondary"
            onclick={() => onShowSource(attempt.recovery_from!)}
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
              onShowConversation(
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
      <dt>Observed charge</dt>
      <dd>
        {allowanceChargeSummary(attempt)}. A reported charge counts toward plan
        usage limits as reported, and never states remaining allowance
      </dd>
      {#if attempt.paid_binding_id}
        <dt>Paid accounting</dt>
        <dd>{attempt.paid_monetary_state ?? 'unknown'}</dd>
      {/if}
      <dt>Input / output</dt>
      <dd>
        {tokenCount(attempt.input_tokens)} / {tokenCount(attempt.output_tokens)}
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
        including after disconnect or restart. OLP does not retry or replay this
        request.
      </p>{/if}
  </article>
{/each}

<style>
  article {
    display: grid;
    gap: 1rem;
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
  @media (max-width: 40rem) {
    dl {
      grid-template-columns: 1fr;
    }
  }
</style>
