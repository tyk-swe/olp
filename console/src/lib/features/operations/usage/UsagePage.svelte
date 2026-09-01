<script lang="ts">
  import { createQuery } from '@tanstack/svelte-query';
  import { queryKeys } from '$lib/api/queryKeys';
  import {
    usageBreakdown,
    usageCompleteness,
    usageSeries,
    usageSummary,
    type UsageFilters
  } from '$lib/api/usage';
  import { getApiKey } from '$lib/api/management/api-keys';
  import UsageChart from './UsageChart.svelte';
  import UsageCompletenessStatus from './UsageCompletenessStatus.svelte';
  import { errorMessage } from '$lib/api/http';
  import {
    dateTimeLocalValue,
    formatBudget,
    formatCompact,
    formatCost,
    formatDate,
    formatInteger
  } from '$lib/format';

  type Dimension = 'route' | 'provider' | 'model' | 'api_key' | 'operation';

  const now = new Date();
  const yesterday = new Date(now.valueOf() - 24 * 60 * 60 * 1000);
  let start = $state(dateTimeLocalValue(yesterday));
  let end = $state(dateTimeLocalValue(now));
  let route = $state('');
  let model = $state('');
  let providerId = $state('');
  let apiKeyId = $state('');
  let operation = $state('');
  let dimension = $state<Dimension>('route');
  let granularity = $state<'hour' | 'day'>('hour');
  let applied = $state<{
    filters: UsageFilters;
    dimension: Dimension;
    granularity: 'hour' | 'day';
  }>({
    filters: { start: yesterday.toISOString(), end: now.toISOString() },
    dimension: 'route',
    granularity: 'hour'
  });

  const usage = createQuery(() => ({
    queryKey: queryKeys.usage.report(JSON.stringify(applied)),
    queryFn: async ({ signal }) => {
      const [summary, series, breakdown, completeness, apiKey] =
        await Promise.all([
          usageSummary(applied.filters),
          usageSeries(applied.filters, applied.granularity),
          usageBreakdown(applied.filters, applied.dimension),
          usageCompleteness(applied.filters),
          applied.filters.api_key_id
            ? getApiKey(applied.filters.api_key_id, signal)
            : Promise.resolve(null)
        ]);
      return {
        summary,
        points: series.items,
        breakdown: breakdown.items,
        completeness,
        apiKey
      };
    }
  }));

  function apply(event: SubmitEvent) {
    event.preventDefault();
    applied = {
      filters: {
        start: new Date(start).toISOString(),
        end: new Date(end).toISOString(),
        route: route || undefined,
        model: model || undefined,
        provider_id: providerId || undefined,
        api_key_id: apiKeyId || undefined,
        operation: operation || undefined
      },
      dimension,
      granularity
    };
  }

  function clear() {
    const resetEnd = new Date();
    const resetStart = new Date(resetEnd.valueOf() - 24 * 60 * 60 * 1000);
    start = dateTimeLocalValue(resetStart);
    end = dateTimeLocalValue(resetEnd);
    route = model = providerId = apiKeyId = operation = '';
    applied = {
      filters: { start: resetStart.toISOString(), end: resetEnd.toISOString() },
      dimension,
      granularity
    };
  }

  function titleCase(value: string) {
    return value.charAt(0).toUpperCase() + value.slice(1);
  }
</script>

<svelte:head><title>Usage · OpenLLMProxy</title></svelte:head>

<div class="page-header">
  <div>
    <p class="eyebrow">Operations</p>
    <h1 class="page-title">Usage</h1>
    <p class="page-description">
      Traffic, tokens, media units, and estimated costs with visible pricing and
      ingestion completeness.
    </p>
  </div>
  <button
    class="button button-secondary"
    type="button"
    onclick={() => usage.refetch()}
    disabled={usage.isFetching}>Refresh</button
  >
</div>

<form class="card filters" aria-label="Usage filters" onsubmit={apply}>
  <div class="filter-grid">
    <label
      >From <input bind:value={start} type="datetime-local" required /></label
    >
    <label>To <input bind:value={end} type="datetime-local" required /></label>
    <label>Route <input bind:value={route} placeholder="All routes" /></label>
    <label
      >Provider ID <input
        bind:value={providerId}
        class="mono"
        placeholder="All providers"
      /></label
    >
    <label>Model <input bind:value={model} placeholder="All models" /></label>
    <label
      >API key ID <input
        bind:value={apiKeyId}
        class="mono"
        placeholder="All keys"
      /></label
    >
    <label
      >Operation <input
        bind:value={operation}
        placeholder="All operations"
      /></label
    >
    <label
      >Break down by <select bind:value={dimension}
        ><option value="route">Route</option><option value="provider"
          >Provider</option
        ><option value="model">Model</option><option value="api_key"
          >API key</option
        ><option value="operation">Operation</option></select
      ></label
    >
    <label
      >Time buckets <select bind:value={granularity}
        ><option value="hour">Hourly</option><option value="day">Daily</option
        ></select
      ></label
    >
  </div>
  <div class="filter-actions">
    <button class="button button-primary" type="submit">Apply</button><button
      class="button button-secondary"
      type="button"
      onclick={clear}>Reset</button
    >
  </div>
</form>

{#if usage.isPending}
  <div class="loading-state" role="status">Calculating usage…</div>
{:else if usage.isError}
  <div class="inline-problem" role="alert">
    {errorMessage(usage.error, 'Usage could not be loaded.')}
    <button class="text-button" onclick={() => usage.refetch()}
      >Try again</button
    >
  </div>
{:else if usage.data}
  <UsageCompletenessStatus completeness={usage.data.completeness} />

  {#if usage.data.apiKey}
    <section class="card budget-line" aria-label="Filtered API key budget">
      <div class="budget-key">
        <p>API key budget</p>
        <strong>{usage.data.apiKey.name}</strong>
        <span class="mono">{usage.data.apiKey.id}</span>
      </div>
      <div>
        <p>Daily accrued / limit</p>
        <strong
          >{formatBudget(
            usage.data.apiKey.budget.daily.accrued,
            usage.data.summary.currency
          )} / {usage.data.apiKey.budget.daily.limit === null
            ? 'No limit'
            : formatBudget(
                usage.data.apiKey.budget.daily.limit,
                usage.data.summary.currency
              )}</strong
        >
        <span
          >Window ends {formatDate(
            usage.data.apiKey.budget.daily.window_ends_at
          )}</span
        >
      </div>
      <div>
        <p>Monthly accrued / limit</p>
        <strong
          >{formatBudget(
            usage.data.apiKey.budget.monthly.accrued,
            usage.data.summary.currency
          )} / {usage.data.apiKey.budget.monthly.limit === null
            ? 'No limit'
            : formatBudget(
                usage.data.apiKey.budget.monthly.limit,
                usage.data.summary.currency
              )}</strong
        >
        <span
          >Window ends {formatDate(
            usage.data.apiKey.budget.monthly.window_ends_at
          )}</span
        >
      </div>
      <div>
        <p>Unpriced attempts this UTC month</p>
        <strong
          >{formatInteger(usage.data.apiKey.budget.unpriced_attempts)}</strong
        >
        <span>Unpriced attempts accrue 0.</span>
      </div>
    </section>
  {/if}

  <section
    class="pipeline-grid"
    aria-label="Request metadata persistence and usage range coverage"
  >
    <article class="card pipeline-card">
      <p>Request metadata consumer</p>
      <strong
        class:danger-text={usage.data.completeness.request_metadata_consumer
          .state === 'stale'}
        >{titleCase(
          usage.data.completeness.request_metadata_consumer.state
        )}</strong
      >
      <span
        >{!usage.data.completeness.request_metadata_consumer.checked_at
          ? 'No worker checkpoint recorded'
          : usage.data.completeness.request_metadata_consumer
                .heartbeat_age_seconds == null
            ? 'Checkpoint age unknown'
            : `Checkpoint ${usage.data.completeness.request_metadata_consumer.heartbeat_age_seconds}s ago`}</span
      >
    </article>
    <article class="card pipeline-card">
      <p>Pending acknowledgements</p>
      <strong
        >{formatCompact(
          usage.data.completeness.request_metadata_consumer.pending_events
        )}</strong
      >
      <span
        >{usage.data.completeness.request_metadata_consumer.oldest_pending_at
          ? 'Oldest pending event is tracked'
          : 'No delivered events waiting'}</span
      >
    </article>
    <article class="card pipeline-card">
      <p>Stream lag</p>
      <strong
        >{formatCompact(
          usage.data.completeness.request_metadata_consumer.lag_events
        )}</strong
      >
      <span>Events not yet delivered to the worker</span>
    </article>
    <article class="card pipeline-card">
      <p>Range coverage</p>
      <strong class:danger-text={usage.data.completeness.coverage.approximate}
        >{usage.data.completeness.coverage.range_complete
          ? 'Exact'
          : 'Incomplete'}</strong
      >
      <span
        >{usage.data.completeness.coverage.approximate
          ? 'Approximate totals · '
          : ''}{usage.data.completeness.coverage
          .excluded_partial_aggregate_boundaries} partial retained-hour {usage
          .data.completeness.coverage.excluded_partial_aggregate_boundaries ===
        1
          ? 'boundary'
          : 'boundaries'} excluded</span
      >
    </article>
    <article class="card pipeline-card">
      <p>Priced requests</p>
      <strong>{formatCompact(usage.data.completeness.priced_count)}</strong>
      <span
        >of {formatCompact(usage.data.completeness.request_count)} requests in range</span
      >
    </article>
    <article class="card pipeline-card">
      <p>Gateway epoch uncertainty</p>
      <strong
        class:danger-text={usage.data.completeness
          .uncertain_request_metadata_gap_count > 0}
        >{formatCompact(
          usage.data.completeness.uncertain_request_metadata_gap_count
        )}</strong
      >
      <span>Unclean process epochs with an unknown exact loss count</span>
    </article>
  </section>

  <section class="metric-grid usage-metrics" aria-label="Usage summary">
    <article class="card metric-card">
      <p>Requests</p>
      <strong>{formatCompact(usage.data.summary.request_count)}</strong>
    </article>
    <article class="card metric-card">
      <p>Input / output tokens</p>
      <strong
        >{formatCompact(usage.data.summary.input_tokens)} / {formatCompact(
          usage.data.summary.output_tokens
        )}</strong
      >
    </article>
    <article class="card metric-card">
      <p>Cached input tokens</p>
      <strong>{formatCompact(usage.data.summary.cached_input_tokens)}</strong>
    </article>
    <article class="card metric-card">
      <p>Media units</p>
      <strong>{formatCompact(usage.data.summary.media_units)}</strong>
    </article>
    <article class="card metric-card">
      <p>Estimated cost</p>
      <strong class:unpriced={usage.data.summary.estimated_cost == null}
        >{formatCost(
          usage.data.summary.estimated_cost,
          usage.data.summary.currency
        )}</strong
      >
    </article>
  </section>

  <UsageChart
    points={usage.data.points}
    granularity={applied.granularity}
    title={applied.granularity === 'day'
      ? 'Requests per day'
      : 'Requests per hour'}
  />

  <section class="breakdown" aria-labelledby="breakdown-title">
    <div class="section-heading">
      <div>
        <p class="eyebrow">Breakdown</p>
        <h2 id="breakdown-title">By {applied.dimension.replace('_', ' ')}</h2>
      </div>
      <span class="badge">Top {usage.data.breakdown.length}</span>
    </div>
    {#if usage.data.breakdown.length === 0}
      <div class="card empty-state">No usage in this time range.</div>
    {:else}
      <!-- svelte-ignore a11y_no_noninteractive_tabindex -->
      <div
        class="table-shell"
        tabindex="0"
        role="region"
        aria-label="Usage breakdown"
      >
        <table class="data-table">
          <caption class="sr-only"
            >Usage breakdown by {applied.dimension}</caption
          >
          <thead
            ><tr
              ><th scope="col">{applied.dimension.replace('_', ' ')}</th><th
                scope="col">Requests</th
              ><th scope="col">Input tokens</th><th scope="col">Cached input</th
              ><th scope="col">Output tokens</th><th scope="col"
                >Estimated cost</th
              ><th scope="col">Completeness</th></tr
            ></thead
          >
          <tbody
            >{#each usage.data.breakdown as row (row.dimension)}<tr
                ><td><strong>{row.dimension}</strong></td><td
                  >{formatCompact(row.request_count)}</td
                ><td>{formatCompact(row.input_tokens)}</td><td
                  >{formatCompact(row.cached_input_tokens)}</td
                ><td>{formatCompact(row.output_tokens)}</td><td
                  >{formatCost(row.estimated_cost, row.currency)}</td
                ><td
                  >{#if row.incomplete_count > 0}<span class="badge danger"
                      >{row.incomplete_count} incomplete</span
                    >{:else if row.unpriced_count > 0}<span
                      class="badge warning">{row.unpriced_count} unpriced</span
                    >{:else}<span class="badge success">Complete</span>{/if}</td
                ></tr
              >{/each}</tbody
          >
        </table>
      </div>
    {/if}
  </section>
{/if}

<style>
  .filters {
    margin-top: 1.5rem;
    padding: 1rem;
  }
  .filter-grid {
    display: grid;
    grid-template-columns: repeat(3, minmax(0, 1fr));
    gap: 0.8rem;
  }
  label {
    display: grid;
    gap: 0.35rem;
    color: var(--foreground-muted);
    font-size: 0.75rem;
    font-weight: 700;
  }
  input,
  select {
    width: 100%;
    min-height: 2.5rem;
    padding: 0.5rem 0.7rem;
    border: 1px solid var(--border-strong);
    border-radius: 0.375rem;
    background: var(--surface);
    color: var(--foreground);
  }
  .filter-actions {
    display: flex;
    gap: 0.65rem;
    margin-top: 1rem;
  }
  /* Five summary cards do not divide into the shared four-column grid, so this
     page fits as many as the row holds. Scoped to its own class and its own
     widths so Health and Models keep the shared 4 -> 2 -> 1 rhythm, and so the
     app.css breakpoints below still take over on narrow screens. */
  @media (min-width: 56.0625rem) {
    .metric-grid.usage-metrics {
      grid-template-columns: repeat(auto-fit, minmax(10.5rem, 1fr));
    }
  }
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
    padding: 1rem;
  }
  .budget-line > div {
    display: grid;
    align-content: start;
    gap: 0.2rem;
  }
  .budget-line p,
  .budget-line span {
    margin: 0;
    color: var(--foreground-muted);
    font-size: 0.75rem;
  }
  .budget-key .mono {
    overflow-wrap: anywhere;
  }
  .pipeline-card {
    display: grid;
    gap: 0.2rem;
    padding: 0.9rem 1rem;
  }
  .pipeline-card p,
  .pipeline-card span {
    margin: 0;
    color: var(--foreground-muted);
  }
  .pipeline-card p {
    font-size: 0.75rem;
    font-weight: 700;
  }
  .pipeline-card strong {
    font-size: 1.1rem;
  }
  .pipeline-card span {
    font-size: 0.78rem;
  }
  .danger-text {
    color: var(--danger);
  }
  .unpriced {
    color: var(--warning);
  }
  .breakdown {
    margin-top: 2rem;
  }
  .section-heading {
    display: flex;
    align-items: flex-start;
    justify-content: space-between;
    gap: 1rem;
    margin-bottom: 0.75rem;
  }
  h2 {
    margin: 0;
    font-size: 1.2rem;
  }
  @media (max-width: 68rem) {
    .filter-grid,
    .pipeline-grid,
    .budget-line {
      grid-template-columns: repeat(2, minmax(0, 1fr));
    }
  }
  @media (max-width: 40rem) {
    .filter-grid,
    .pipeline-grid,
    .budget-line {
      grid-template-columns: 1fr;
    }
    .filters {
      padding: 0.85rem;
    }
  }
</style>
