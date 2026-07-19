<script lang="ts">
  import { Axis, Chart, Layer, Spline } from 'layerchart';
  import type { UsagePoint } from '$lib/api/operations';
  import { formatCompact, formatCost, formatDate } from './format';

  let { points, title = 'Requests over time' }: { points: UsagePoint[]; title?: string } = $props();
  const data = $derived(points.map((point) => ({ ...point, time: new Date(point.bucket) })));
</script>

<figure class="usage-chart" aria-labelledby="usage-chart-title" aria-describedby="usage-chart-description">
  <figcaption>
    <div><h2 id="usage-chart-title">{title}</h2><p id="usage-chart-description">Request count by time bucket. Exact values follow the chart.</p></div>
    <span class="legend"><span aria-hidden="true"></span>Requests</span>
  </figcaption>
  {#if data.length === 0}
    <div class="empty-state">No usage was recorded in this time range.</div>
  {:else}
    <div class="chart" aria-hidden="true">
      <Chart {data} x="time" y="request_count" padding={{ left: 52, right: 16, top: 12, bottom: 36 }}>
        <Layer type="svg">
          <Axis placement="left" grid rule />
          <Axis placement="bottom" />
          <Spline stroke="var(--accent)" strokeWidth={2.5} />
        </Layer>
      </Chart>
    </div>
    <details>
      <summary>View chart data</summary>
      <!-- svelte-ignore a11y_no_noninteractive_tabindex -->
      <div class="table-shell" tabindex="0" role="region" aria-label="Chart data table">
        <table class="data-table">
          <caption class="sr-only">Exact usage values shown in the time-series chart</caption>
          <thead><tr><th scope="col">Bucket</th><th scope="col">Requests</th><th scope="col">Input tokens</th><th scope="col">Output tokens</th><th scope="col">Estimated cost</th><th scope="col">Status</th></tr></thead>
          <tbody>
            {#each points as point (point.bucket)}
              <tr><td>{formatDate(point.bucket)}</td><td>{point.request_count}</td><td>{point.input_tokens}</td><td>{point.output_tokens}</td><td>{formatCost(point.estimated_cost, point.currency ?? 'USD')}</td><td>{point.incomplete_count > 0 ? `${point.incomplete_count} incomplete` : point.unpriced_count > 0 ? `${point.unpriced_count} unpriced` : 'Complete'}</td></tr>
            {/each}
          </tbody>
        </table>
      </div>
    </details>
  {/if}
</figure>

<style>
  .usage-chart { margin: 1rem 0 0; padding: 1.25rem; border: 1px solid var(--border); border-radius: 0.5rem; background: var(--surface); box-shadow: var(--shadow-sm); }
  figcaption { display: flex; align-items: flex-start; justify-content: space-between; gap: 1rem; }
  h2 { margin: 0; font-size: 1.05rem; }
  figcaption p { margin: 0.25rem 0 0; color: var(--foreground-muted); font-size: 0.78rem; }
  .legend { display: inline-flex; align-items: center; gap: 0.4rem; color: var(--foreground-muted); font-size: 0.75rem; }
  .legend span { width: 1rem; height: 0.2rem; border-radius: 1rem; background: var(--accent); }
  .chart { width: 100%; height: 20rem; margin-top: 1rem; color: var(--foreground-muted); }
  details { margin-top: 0.5rem; }
  summary { display: inline-flex; min-height: 2.75rem; align-items: center; color: var(--accent-strong); font-weight: 700; cursor: pointer; }
  @media (max-width: 36rem) { .usage-chart { padding: 0.85rem; } .chart { height: 15rem; } figcaption { display: grid; } }
  @media (forced-colors: active) { .legend span { background: CanvasText; } }
</style>
