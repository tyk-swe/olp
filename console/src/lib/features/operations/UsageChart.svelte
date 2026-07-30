<script lang="ts">
  import type { UsagePoint } from '$lib/api/operations';
  import { formatCompact, formatCost, formatDate } from './format';

  let { points, title = 'Requests over time' }: { points: UsagePoint[]; title?: string } = $props();
  const bounds = { width: 640, height: 260, left: 52, right: 16, top: 12, bottom: 36 };
  const chart = $derived.by(() => {
    const width = bounds.width - bounds.left - bounds.right;
    const height = bounds.height - bounds.top - bounds.bottom;
    const maximum = Math.max(1, ...points.map((point) => point.request_count));
    const step = Math.max(1, Math.ceil(maximum / 4));
    const ceiling = step * 4;
    const coordinates = points.map((point, index) => ({
      x: bounds.left + (index * width) / Math.max(1, points.length - 1),
      y: bounds.top + height - (point.request_count * height) / ceiling
    }));
    return {
      coordinates,
      polyline: coordinates.map(({ x, y }) => `${x},${y}`).join(' '),
      ticks: Array.from({ length: 5 }, (_, index) => ({
        y: bounds.top + (index * height) / 4,
        value: step * (4 - index)
      }))
    };
  });
</script>

<figure class="usage-chart" aria-labelledby="usage-chart-title" aria-describedby="usage-chart-description">
  <figcaption>
    <div><h2 id="usage-chart-title">{title}</h2><p id="usage-chart-description">Request count by time bucket. Exact values follow the chart.</p></div>
    <span class="legend"><span aria-hidden="true"></span>Requests</span>
  </figcaption>
  {#if points.length === 0}
    <div class="empty-state">No usage was recorded in this time range.</div>
  {:else}
    <div class="chart" aria-hidden="true">
      <svg viewBox={`0 0 ${bounds.width} ${bounds.height}`} preserveAspectRatio="none">
        {#each chart.ticks as tick (tick.y)}
          <line class="grid" x1={bounds.left} x2={bounds.width - bounds.right} y1={tick.y} y2={tick.y} />
          <text class="axis-label" x={bounds.left - 8} y={tick.y + 4} text-anchor="end">{formatCompact(tick.value)}</text>
        {/each}
        <polyline class="series" points={chart.polyline} />
        {#each chart.coordinates as point, index (points[index].bucket)}
          <circle class="point" cx={point.x} cy={point.y} r="2.5" />
        {/each}
        <text class="axis-label" x={bounds.left} y={bounds.height - 8}>{formatDate(points[0].bucket)}</text>
        {#if points.length > 1}
          <text class="axis-label" x={bounds.width - bounds.right} y={bounds.height - 8} text-anchor="end">{formatDate(points.at(-1)?.bucket)}</text>
        {/if}
      </svg>
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
  svg { width: 100%; height: 100%; overflow: visible; }
  .grid { stroke: var(--border); stroke-width: 1; vector-effect: non-scaling-stroke; }
  .series { fill: none; stroke: var(--accent); stroke-width: 2.5; stroke-linejoin: round; stroke-linecap: round; vector-effect: non-scaling-stroke; }
  .point { fill: var(--accent); }
  .axis-label { fill: currentColor; font-size: 11px; }
  details { margin-top: 0.5rem; }
  summary { display: inline-flex; min-height: 2.75rem; align-items: center; color: var(--accent-strong); font-weight: 700; cursor: pointer; }
  @media (max-width: 36rem) { .usage-chart { padding: 0.85rem; } .chart { height: 15rem; } figcaption { display: grid; } }
  @media (forced-colors: active) { .legend span { background: CanvasText; } }
</style>
