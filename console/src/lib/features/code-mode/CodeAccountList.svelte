<script lang="ts">
  import AccountDrainControl from './AccountDrainControl.svelte';
  import AccountStatus from './AccountStatus.svelte';
  import { formatDate } from '#lib/format.js';
  import {
    adapterLabel,
    allowanceEvidenceLabel,
    allowanceWindowSummary,
    cooldownSummary,
    extraUsageLabel,
    showsExtraUsage,
    nextMonthlyReset,
    servingHealthLabel,
    tokenCount
  } from './presentation';
  import type { CodeAccount } from '#lib/features/code-mode/api.js';

  let {
    items,
    allowed,
    busy,
    onEdit
  }: {
    items: CodeAccount[];
    allowed: boolean;
    busy: boolean;
    onEdit: (account: CodeAccount) => void;
  } = $props();
</script>

<p>
  Eligibility depends on grant authority, serving cooldowns and observed
  allowance. Unknown serving health means no qualified observation. Enrollment
  is not an inference test. A rejected refresh lapses the grant and requires
  enrollment again. Passing a retry or reset time permits a new attempt without
  changing the reported allowance. Provider allowance is separate from local
  budgets and reported usage
</p>
{#each items as account (account.id)}
  <article class="card">
    <div class="actions">
      <h3>{account.name}</h3>
      {#if allowed}<button
          class="button button-secondary"
          type="button"
          disabled={busy}
          onclick={() => onEdit(account)}>Edit account</button
        ><AccountDrainControl {account} disabled={busy} />{/if}
    </div>
    <dl>
      <dt>Subscription</dt>
      <dd><span class="badge">{adapterLabel(account.adapter)}</span></dd>
      {#if showsExtraUsage(account.adapter)}
        <dt>Extra Usage</dt>
        <dd>{extraUsageLabel(account.extra_usage_enabled)}</dd>
      {/if}
      <dt>Ordering signal</dt>
      <dd>
        No qualified model-to-limit mapping. Legacy eligibility and weighted
        selection apply.
      </dd>
      <dt>Account / principal</dt>
      <dd>{account.id} / {account.principal}</dd>
      <dt>Maximum simultaneous generations</dt>
      <dd>{account.max_concurrency ?? 'Not set'}</dd>
      <AccountStatus {account} />
      <dt>Grant state</dt>
      <dd>
        {account.grant_state === 'lapsed'
          ? 'Grant lapsed, enroll again'
          : account.grant_state}
      </dd>
      <dt>Serving health</dt>
      <dd>
        {servingHealthLabel(account.health)}
        {#if account.health_observed_at}
          · Observed {formatDate(account.health_observed_at)}
        {/if}
      </dd>
      <dt>Serving retry time</dt>
      <dd>
        {account.unavailable_until
          ? formatDate(account.unavailable_until)
          : 'No serving cooldown reported'}
        {#if account.cooldowns?.length}
          {#each account.cooldowns as cooldown (cooldown.class + ':' + (cooldown.model ?? ''))}
            <p>
              {cooldownSummary(cooldown)} · until {formatDate(
                cooldown.retry_at
              )}
            </p>
          {/each}
        {/if}
      </dd>
      {#if account.cycle_anchor}
        <dt>Subscription cycle</dt>
        <dd>
          Anchor {account.cycle_anchor} · Monthly reset {nextMonthlyReset(
            account.cycle_anchor,
            new Date()
          ) ?? 'Unknown'}
        </dd>
      {/if}
      <dt>Native models</dt>
      <dd>{account.models.join(', ')}</dd>
      <dt>Provider / credential</dt>
      <dd>{account.provider_id} / {account.credential_id}</dd>
      <dt>Provider-reported allowance</dt>
      <dd>
        {#if account.allowance}
          {#each [{ label: 'Tokens', value: account.allowance.remaining_tokens, observation: account.allowance.token_observation ?? account.allowance }, { label: 'Requests', value: account.allowance.remaining_requests, observation: account.allowance.request_observation ?? account.allowance }] as count (count.label)}
            <p>
              {count.label}: {tokenCount(count.value)}
              {#if count.value != null}
                · Observed {formatDate(count.observation.observed_at)}
                · Reset {count.observation.resets_at
                  ? formatDate(count.observation.resets_at)
                  : 'Unknown'}
              {/if}
            </p>
          {/each}
          {#if account.allowance.windows?.length}
            {#each account.allowance.windows as window (window.limit_id + ':' + window.window)}
              <p>
                {window.limit_id} / {window.window}: {allowanceWindowSummary(
                  window
                )}<br />
                Window: {window.window_minutes == null
                  ? 'Unknown'
                  : window.window_minutes + ' minutes'} · Reset {window.resets_at
                  ? formatDate(window.resets_at)
                  : 'Unknown'}<br />
                {allowanceEvidenceLabel(window.evidence)} · Observed {formatDate(
                  window.observed_at
                )}
              </p>
            {/each}
          {:else}Percent: {account.allowance.remaining_percent == null
              ? 'Unknown'
              : account.allowance.remaining_percent + '%'}<br />Observed {formatDate(
              account.allowance.observed_at
            )} · Reset {account.allowance.resets_at
              ? formatDate(account.allowance.resets_at)
              : 'Unknown'}{/if}
        {:else}Unknown — no provider observation{/if}
      </dd>
      {#if account.allowance?.credits}
        <dt>Provider-reported credits</dt>
        <dd>
          Has credits: {account.allowance.credits.has_credits ? 'Yes' : 'No'} · Unlimited:
          {account.allowance.credits.unlimited ? 'Yes' : 'No'} · Balance: {account
            .allowance.credits.balance ?? 'Unknown'}<br />
          Observed {formatDate(account.allowance.credits.observed_at)}. Credits
          do not override exhausted subscription windows.
        </dd>
      {/if}
    </dl>
  </article>
{/each}

<style>
  .actions {
    display: flex;
    gap: 0.65rem;
    align-items: center;
    flex-wrap: wrap;
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
  @media (max-width: 40rem) {
    dl {
      grid-template-columns: 1fr;
    }
  }
</style>
