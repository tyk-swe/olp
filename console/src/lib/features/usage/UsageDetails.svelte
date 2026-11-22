<script lang="ts">
  import Advanced from '#lib/components/Advanced.svelte';
  import {
    formatBudget,
    formatCompact,
    formatDate,
    formatInteger
  } from '#lib/format.js';
  import {
    budgetStateNote,
    budgetWindowState
  } from '#lib/features/access/index.js';
  import { presentUsageCompleteness } from './completenessPresentation.js';
  import type { getApiKey } from '#lib/features/access/index.js';
  import {
    usageSummary,
    type UsageCompleteness
  } from '#lib/features/usage/usage.js';

  let {
    apiKey,
    completeness,
    summary
  }: {
    apiKey: Awaited<ReturnType<typeof getApiKey>> | null;
    completeness: UsageCompleteness;
    summary: Pick<Awaited<ReturnType<typeof usageSummary>>, 'currency'>;
  } = $props();

  function titleCase(value: string) {
    return value.charAt(0).toUpperCase() + value.slice(1);
  }
</script>

{#if apiKey}
  {@const budget = apiKey.budget}
  {@const currency = summary.currency}
  {@const daily = budgetWindowState(budget, 'daily')}
  {@const monthly = budgetWindowState(budget, 'monthly')}
  {@const dailyNote = budgetStateNote(daily)}
  {@const monthlyNote = budgetStateNote(monthly)}
  <section class="card budget-line" aria-label="Filtered API key budget">
    <div class="budget-key">
      <p>API key budget</p>
      <strong>{apiKey.name}</strong>
      <span class="mono">{apiKey.id}</span>
    </div>
    <div>
      <p>Daily accrued / limit</p>
      <strong class:danger-text={daily === 'exhausted'}
        >{formatBudget(budget.daily.accrued, currency)} / {budget.daily
          .limit === null
          ? 'No limit'
          : formatBudget(budget.daily.limit, currency)}</strong
      >
      <span>Window ends {formatDate(budget.daily.window_ends_at)}</span>
      {#if dailyNote}<span class:danger-text={daily === 'exhausted'}
          >{dailyNote}</span
        >{/if}
    </div>
    <div>
      <p>Monthly accrued / limit</p>
      <strong class:danger-text={monthly === 'exhausted'}
        >{formatBudget(budget.monthly.accrued, currency)} / {budget.monthly
          .limit === null
          ? 'No limit'
          : formatBudget(budget.monthly.limit, currency)}</strong
      >
      <span>Window ends {formatDate(budget.monthly.window_ends_at)}</span>
      {#if monthlyNote}<span class:danger-text={monthly === 'exhausted'}
          >{monthlyNote}</span
        >{/if}
    </div>
    <div>
      <p>Unpriced attempts this UTC month</p>
      <strong class:unpriced={budget.unpriced_attempts > 0}
        >{formatInteger(budget.unpriced_attempts)}</strong
      >
      <span>Unpriced attempts accrue 0.</span>
    </div>
  </section>
{/if}

<Advanced
  label="Data pipeline details"
  open={presentUsageCompleteness(completeness).kind !== 'complete'}
>
  <section
    class="pipeline-grid"
    aria-label="Request metadata persistence and usage range coverage"
  >
    <article class="card pipeline-card">
      <p>Request metadata consumer</p>
      <strong
        class:danger-text={completeness.request_metadata_consumer.state ===
          'stale'}
        >{titleCase(completeness.request_metadata_consumer.state)}</strong
      >
      <span
        >{!completeness.request_metadata_consumer.checked_at
          ? 'No worker checkpoint recorded'
          : completeness.request_metadata_consumer.heartbeat_age_seconds == null
            ? 'Checkpoint age unknown'
            : `Checkpoint ${completeness.request_metadata_consumer.heartbeat_age_seconds}s ago`}</span
      >
    </article>
    <article class="card pipeline-card">
      <p>Pending acknowledgements</p>
      <strong
        >{formatCompact(
          completeness.request_metadata_consumer.pending_events
        )}</strong
      >
      <span
        >{completeness.request_metadata_consumer.oldest_pending_at
          ? 'Oldest pending event is tracked'
          : 'No delivered events waiting'}</span
      >
    </article>
    <article class="card pipeline-card">
      <p>Stream lag</p>
      <strong
        >{formatCompact(
          completeness.request_metadata_consumer.lag_events
        )}</strong
      >
      <span>Events not yet delivered to the worker</span>
    </article>
    <article class="card pipeline-card">
      <p>Range coverage</p>
      <strong class:danger-text={completeness.coverage.approximate}
        >{completeness.coverage.range_complete ? 'Exact' : 'Incomplete'}</strong
      >
      <span
        >{completeness.coverage.approximate
          ? 'Approximate totals · '
          : ''}{completeness.coverage.excluded_partial_aggregate_boundaries} partial
        retained-hour {completeness.coverage
          .excluded_partial_aggregate_boundaries === 1
          ? 'boundary'
          : 'boundaries'} excluded</span
      >
    </article>
    <article class="card pipeline-card">
      <p>Priced requests</p>
      <strong>{formatCompact(completeness.priced_count)}</strong>
      <span
        >of {formatCompact(completeness.request_count)} requests in range</span
      >
    </article>
    <article class="card pipeline-card">
      <p>Gateway epoch uncertainty</p>
      <strong
        class:danger-text={completeness.uncertain_request_metadata_gap_count >
          0}
        >{formatCompact(
          completeness.uncertain_request_metadata_gap_count
        )}</strong
      >
      <span>Unclean process epochs with an unknown exact loss count</span>
    </article>
  </section>
</Advanced>

<style>
  .pipeline-grid {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(12rem, 1fr));
    gap: 0.75rem;
    margin-top: 1rem;
  }
  .budget-line {
    display: grid;
    grid-template-columns: repeat(4, minmax(0, 1fr));
    gap: 1rem;
    margin-top: 1rem;
    padding: 1.5rem;
  }
  .budget-line > div {
    display: grid;
    align-content: start;
    gap: 0.2rem;
  }
  .budget-line p {
    margin: 0;
    color: var(--foreground-subtle);
    font-family: var(--font-mono);
    font-size: var(--text-caption);
    font-weight: 400;
    letter-spacing: -0.02em;
    text-transform: uppercase;
  }
  .budget-line span {
    margin: 0;
    color: var(--foreground-muted);
    font-size: var(--text-caption);
  }
  .budget-key .mono {
    overflow-wrap: anywhere;
  }
  /* Wins over the muted caption colour the notes would otherwise inherit. */
  .budget-line .danger-text {
    color: var(--danger);
  }
  .pipeline-card {
    display: grid;
    gap: 0.2rem;
    padding: 1rem 1.25rem;
  }
  .pipeline-card p {
    margin: 0;
    color: var(--foreground-subtle);
    font-family: var(--font-mono);
    font-size: var(--text-caption);
    font-weight: 400;
    letter-spacing: -0.02em;
    text-transform: uppercase;
  }
  .pipeline-card span {
    margin: 0;
    color: var(--foreground-muted);
    font-size: var(--text-caption);
  }
  .pipeline-card strong {
    font-size: 1.25rem;
    font-weight: 400;
    letter-spacing: -0.02em;
  }
  .danger-text {
    color: var(--danger);
  }
  @media (max-width: 68rem) {
    .pipeline-grid,
    .budget-line {
      grid-template-columns: repeat(2, minmax(0, 1fr));
    }
  }
  @media (max-width: 40rem) {
    .pipeline-grid,
    .budget-line {
      grid-template-columns: 1fr;
    }
  }
</style>
