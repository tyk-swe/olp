<script lang="ts">
  import ScrollTable from '#lib/components/ScrollTable.svelte';
  import EmptyState from '#lib/components/EmptyState.svelte';
  import UsageChart from '#lib/features/usage/UsageChart.svelte';
  import UnpricedBreakdown from './UnpricedBreakdown.svelte';
  import { presentEstimate } from '#lib/features/usage/estimatePresentation.js';
  import { formatCompact, formatCost } from '#lib/format.js';
  import {
    usageSummary,
    usageSeries,
    usageBreakdown
  } from '#lib/features/usage/usage.js';
  import type { UsageState } from '#lib/features/usage/usageState.js';

  type Report = {
    summary: Awaited<ReturnType<typeof usageSummary>>;
    points: Awaited<ReturnType<typeof usageSeries>>['items'];
    breakdown: Awaited<ReturnType<typeof usageBreakdown>>['items'];
  };

  let {
    report,
    applied
  }: {
    report: Pick<Report, 'summary' | 'points' | 'breakdown'>;
    applied: UsageState;
  } = $props();

  const estimate = $derived(presentEstimate(report.summary));
</script>

<section class="metric-grid usage-metrics" aria-label="Usage summary">
  <article class="card metric-card">
    <p>Requests</p>
    <strong>{formatCompact(report.summary.request_count)}</strong>
  </article>
  <article class="card metric-card">
    <p>Input / output tokens</p>
    <strong>
      {formatCompact(report.summary.input_tokens)} / {formatCompact(
        report.summary.output_tokens
      )}
    </strong>
  </article>
  <article class="card metric-card">
    <p>Cached input tokens</p>
    <strong>{formatCompact(report.summary.cached_input_tokens)}</strong>
  </article>
  <article class="card metric-card">
    <p>Cache write tokens</p>
    <strong>{formatCompact(report.summary.cache_write_input_tokens)}</strong>
    <small>
      5m {formatCompact(report.summary.cache_write_5m_input_tokens)} · 1h {formatCompact(
        report.summary.cache_write_1h_input_tokens
      )}
    </small>
  </article>
  <article class="card metric-card">
    <p>Audio / image tokens</p>
    <strong>
      {formatCompact(report.summary.audio_input_tokens)} · {formatCompact(
        report.summary.image_input_tokens
      )}
    </strong>
    <small>
      in — {formatCompact(report.summary.audio_output_tokens)} · {formatCompact(
        report.summary.image_output_tokens
      )} out
    </small>
  </article>
  <article class="card metric-card">
    <p>Media units</p>
    <strong>{formatCompact(report.summary.media_units)}</strong>
  </article>
  <article class="card metric-card">
    <p>Allocated commitment</p>
    <small>
      Provisioned-capacity share priced at zero above, allocated across the
      period's committed usage.
    </small>
    <strong>
      {formatCost(report.summary.allocated_commitment, report.summary.currency)}
    </strong>
  </article>
  <article class="card metric-card">
    <p>Charge</p>
    <small>
      What consumers are billed under the applicable rate card; identical to
      cost when no card applies.
    </small>
    <strong class:unpriced={report.summary.charge == null}>
      {formatCost(report.summary.charge, report.summary.currency)}
    </strong>
    {#if report.summary.uncharged_attempt_count > 0}<span class="badge warning">
        {report.summary.uncharged_attempt_count} uncharged
      </span>{/if}
  </article>
  <article class="card metric-card">
    <p>Estimated cost</p>
    <small>
      Estimate from recorded, priced usage. Review delivery and coverage above;
      this is not an upstream invoice.
    </small>
    <strong class:unpriced={report.summary.estimated_cost == null}>
      {formatCost(report.summary.estimated_cost, report.summary.currency)}
    </strong>
  </article>
  <article class="card metric-card">
    <p>Input estimate error</p>
    <strong>{estimate.error}</strong>
    <small>
      {estimate.note}{#if estimate.hasSample}
        {estimate.comparison} estimated / reported input across {estimate.sample}
        that had both.{/if}
    </small>
  </article>
</section>

<UsageChart
  points={report.points}
  granularity={applied.granularity}
  title={applied.granularity === 'day'
    ? 'Requests per day'
    : 'Requests per hour'}
/>

<UnpricedBreakdown filters={applied.filters} />

<section class="breakdown" aria-labelledby="breakdown-title">
  <div class="section-heading">
    <div>
      <p class="eyebrow">Breakdown</p>
      <h2 id="breakdown-title">
        By {applied.dimension === 'attribution'
          ? `attribution: ${applied.filters.attribution_key ?? ''}`
          : applied.dimension.replace('_', ' ')}
      </h2>
    </div>
    <span class="badge">Top {report.breakdown.length}</span>
  </div>
  {#if report.breakdown.length === 0}
    <EmptyState title="No usage in this time range">
      Try a wider date range or fewer filters.
    </EmptyState>
  {:else}
    <ScrollTable label="Usage breakdown">
      <table class="data-table">
        <caption class="sr-only">
          Usage breakdown by {applied.dimension}
        </caption>
        <thead>
          <tr>
            <th scope="col">{applied.dimension.replace('_', ' ')}</th>
            <th scope="col">Requests</th>
            <th scope="col">Input tokens</th>
            <th scope="col">Cached input</th>
            <th scope="col">Cache write</th>
            <th scope="col">Output tokens</th>
            <th scope="col">Audio tokens</th>
            <th scope="col">Image tokens</th>
            <th scope="col">Allocated commitment</th>
            <th scope="col">Estimated / reported input</th>
            <th scope="col">Estimate error</th>
            <th scope="col">Estimated cost</th>
            <th scope="col">Charge</th>
            <th scope="col">Completeness</th>
          </tr>
        </thead>
        <tbody>
          {#each report.breakdown as row (row.dimension)}
            {@const rowEstimate = presentEstimate(row)}
            <tr>
              <td><strong>{row.dimension}</strong></td>
              <td>{formatCompact(row.request_count)}</td>
              <td>{formatCompact(row.input_tokens)}</td>
              <td>{formatCompact(row.cached_input_tokens)}</td>
              <td>{formatCompact(row.cache_write_input_tokens)}</td>
              <td>{formatCompact(row.output_tokens)}</td>
              <td>
                {formatCompact(row.audio_input_tokens)} / {formatCompact(
                  row.audio_output_tokens
                )}
              </td>
              <td>
                {formatCompact(row.image_input_tokens)} / {formatCompact(
                  row.image_output_tokens
                )}
              </td>
              <td>{formatCost(row.allocated_commitment, row.currency)}</td>
              <td>{rowEstimate.comparison}</td>
              <td>{rowEstimate.error}</td>
              <td>{formatCost(row.estimated_cost, row.currency)}</td>
              <td>
                {formatCost(
                  row.charge,
                  row.currency
                )}{#if row.uncharged_attempt_count > 0}
                  <span class="badge warning">
                    {row.uncharged_attempt_count}
                  </span>
                {/if}
              </td>
              <td>
                {#if row.incomplete_count > 0}
                  <span class="badge danger">
                    {row.incomplete_count} incomplete
                  </span>
                {:else if row.unpriced_count > 0}
                  <span class="badge warning">
                    {row.unpriced_count} unpriced
                  </span>
                {:else}
                  <span class="badge success">Complete</span>
                {/if}
              </td>
            </tr>
          {/each}
        </tbody>
      </table>
    </ScrollTable>
  {/if}
</section>

<style>
  /* Five summary cards do not divide into the shared four-column grid, so this
     page fits as many as the row holds. Scoped to its own class and its own
     widths so Health and Models keep the shared 4 -> 2 -> 1 rhythm, and so the
     app.css breakpoints below still take over on narrow screens. */
  @media (min-width: 56.0625rem) {
    .metric-grid.usage-metrics {
      grid-template-columns: repeat(auto-fit, minmax(10.5rem, 1fr));
    }
  }
  .usage-metrics small {
    display: block;
    margin-top: 0.5rem;
    color: var(--foreground-muted);
    font-size: var(--text-caption);
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
    font-size: var(--text-body);
    font-weight: 500;
    letter-spacing: -0.02em;
  }
</style>
