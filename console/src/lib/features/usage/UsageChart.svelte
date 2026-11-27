<script lang="ts">
  import EmptyState from '#lib/components/EmptyState.svelte';
  import type { UsagePoint } from '#lib/features/usage/usage.js';
  import {
    buildUsageChart,
    nearestCoordinateIndex,
    stepBucketIndex
  } from '#lib/features/usage/usageChart.js';
  import UsageChartTable from './UsageChartTable.svelte';
  import {
    formatCompact,
    formatCost,
    formatDate,
    formatDay
  } from '#lib/format.js';

  let {
    points,
    granularity = 'hour',
    title = 'Requests over time'
  }: {
    points: UsagePoint[];
    granularity?: 'hour' | 'day';
    title?: string;
  } = $props();

  let chartContainer = $state<HTMLDivElement>();
  let chartWidth = $state(640);
  let chartHeight = $state(260);
  let activeIndex = $state<number | null>(null);

  $effect(() => {
    void points;
    activeIndex = null;
  });

  $effect(() => {
    const node = chartContainer;
    if (!node || typeof ResizeObserver === 'undefined') return;
    const observer = new ResizeObserver(([entry]) => {
      const { width, height } = entry.contentRect;
      if (width > 0) chartWidth = Math.max(280, Math.round(width));
      if (height > 0) chartHeight = Math.max(180, Math.round(height));
    });
    observer.observe(node);
    return () => observer.disconnect();
  });

  const bounds = $derived({
    width: chartWidth,
    height: chartHeight,
    left: 52,
    right: 16,
    top: 12,
    bottom: 36
  });
  const uid = $props.id();
  const gradientId = `usage-area-${uid}`;
  const bucketLabel = $derived(granularity === 'day' ? formatDay : formatDate);
  const chart = $derived(buildUsageChart(points, bounds));

  const activePoint = $derived(
    activeIndex !== null && activeIndex >= 0 && activeIndex < points.length
      ? points[activeIndex]
      : null
  );
  const activeCoord = $derived(
    activeIndex !== null &&
      activeIndex >= 0 &&
      activeIndex < chart.coordinates.length
      ? chart.coordinates[activeIndex]
      : null
  );

  function handlePointer(event: PointerEvent) {
    if (!chart.coordinates.length) return;
    const rect = (event.currentTarget as HTMLElement).getBoundingClientRect();
    if (rect.width <= 0) return;
    const targetX = ((event.clientX - rect.left) / rect.width) * bounds.width;
    activeIndex = nearestCoordinateIndex(chart.coordinates, targetX);
  }

  function handleKey(event: KeyboardEvent) {
    const step = stepBucketIndex(activeIndex, points.length, event.key);
    if (!step.handled) return;
    if (event.key !== 'Escape') event.preventDefault();
    activeIndex = step.next;
  }
</script>

<figure
  class="usage-chart"
  aria-labelledby="usage-chart-title"
  aria-describedby="usage-chart-description"
>
  <figcaption>
    <div>
      <h2 id="usage-chart-title">{title}</h2>
      <p id="usage-chart-description">
        Request count by time bucket. Exact values follow the chart.
      </p>
    </div>
    <span class="legend"><span aria-hidden="true"></span>Requests</span>
  </figcaption>
  {#if points.length === 0}
    <div class="chart-empty">
      <EmptyState title="No usage recorded">
        No usage was recorded in this time range.
      </EmptyState>
    </div>
  {:else}
    <div class="readout" aria-live="polite">
      {#if activePoint}
        <span class="readout-time">{bucketLabel(activePoint.bucket)}</span>
        <span>{formatCompact(activePoint.request_count)} req</span>
        <span>
          {formatCompact(activePoint.input_tokens)} in / {formatCompact(
            activePoint.output_tokens
          )} out
        </span>
        <span>
          {formatCost(activePoint.estimated_cost, activePoint.currency)}
        </span>
      {:else}
        <span class="muted">Hover or focus chart to inspect buckets</span>
      {/if}
    </div>
    <!-- svelte-ignore a11y_no_static_element_interactions (The range input inside the chart owns keyboard bucket selection.) -->
    <div
      bind:this={chartContainer}
      class="chart"
      onpointerdown={handlePointer}
      onpointermove={handlePointer}
      onpointerleave={() => (activeIndex = null)}
    >
      <input
        class="chart-scrubber"
        type="range"
        min="0"
        max={Math.max(0, points.length - 1)}
        value={activeIndex ?? 0}
        aria-label="Inspect usage bucket"
        aria-valuetext={activePoint
          ? `${bucketLabel(activePoint.bucket)}, ${formatCompact(activePoint.request_count)} requests`
          : 'No bucket selected'}
        oninput={(event) => (activeIndex = Number(event.currentTarget.value))}
        onblur={() => (activeIndex = null)}
        onkeydown={handleKey}
      />
      <svg viewBox={`0 0 ${bounds.width} ${bounds.height}`} aria-hidden="true">
        <defs>
          <linearGradient id={gradientId} x1="0" x2="0" y1="0" y2="1">
            <stop offset="0" class="area-top" />
            <stop offset="1" class="area-bottom" />
          </linearGradient>
        </defs>
        {#each chart.ticks as tick (tick.y)}
          <line
            class="grid"
            x1={bounds.left}
            x2={bounds.width - bounds.right}
            y1={tick.y}
            y2={tick.y}
          />
          <text
            class="axis-label"
            x={bounds.left - 8}
            y={tick.y + 4}
            text-anchor="end"
          >
            {formatCompact(tick.value)}
          </text>
        {/each}
        {#if activeCoord}
          <line
            class="crosshair"
            x1={activeCoord.x}
            x2={activeCoord.x}
            y1={bounds.top}
            y2={bounds.height - bounds.bottom}
          />
        {/if}
        <g class="plot">
          <polygon
            class="area"
            points={chart.area}
            fill={`url(#${gradientId})`}
          />
          <polyline class="series" points={chart.polyline} />
          {#each chart.coordinates as point, index (points[index].bucket)}
            <circle
              class="point"
              class:active={index === activeIndex}
              cx={point.x}
              cy={point.y}
              r={index === activeIndex ? 4.5 : 2.5}
            />
          {/each}
        </g>
        <text class="axis-label" x={bounds.left} y={bounds.height - 8}>
          {bucketLabel(points[0].bucket)}
        </text>
        {#if points.length > 1}
          <text
            class="axis-label"
            x={bounds.width - bounds.right}
            y={bounds.height - 8}
            text-anchor="end"
          >
            {bucketLabel(points.at(-1)?.bucket)}
          </text>
        {/if}
      </svg>
    </div>
    <UsageChartTable {points} {bucketLabel} />
  {/if}
</figure>

<style>
  .usage-chart {
    margin: 1rem 0 0;
    padding: 1.5rem;
    border: 1px solid var(--border-hairline);
    border-radius: var(--radius-card);
    background: transparent;
  }
  figcaption {
    display: flex;
    align-items: flex-start;
    justify-content: space-between;
    gap: 1rem;
  }
  h2 {
    margin: 0;
    font-size: var(--text-body);
    font-weight: 500;
    letter-spacing: -0.02em;
  }
  figcaption p {
    margin: 0.25rem 0 0;
    color: var(--foreground-muted);
    font-size: var(--text-caption);
  }
  .legend {
    display: inline-flex;
    align-items: center;
    gap: 0.4rem;
    color: var(--foreground-subtle);
    font-family: var(--font-mono);
    font-size: var(--text-caption);
    font-weight: 400;
    letter-spacing: -0.02em;
    text-transform: uppercase;
  }
  .legend span {
    width: 1rem;
    height: 2px;
    background: var(--chart);
  }
  .chart-empty {
    margin-top: 1rem;
  }
  .readout {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 0.4rem 1rem;
    min-height: 1.5rem;
    margin-top: 1rem;
    color: var(--foreground-subtle);
    font-family: var(--font-mono);
    font-size: var(--text-caption);
    font-variant-numeric: tabular-nums;
  }
  .readout-time {
    color: var(--foreground);
  }
  .chart {
    position: relative;
    width: 100%;
    height: 18rem;
    margin-top: 0.35rem;
    border-radius: var(--radius-control);
    cursor: crosshair;
  }
  .chart:has(:focus-visible) {
    outline: 2px solid var(--focus);
    outline-offset: 2px;
  }
  .chart-scrubber {
    position: absolute;
    z-index: 1;
    inset: 0;
    width: 100%;
    height: 100%;
    margin: 0;
    opacity: 0;
    pointer-events: none;
  }
  svg {
    width: 100%;
    height: 100%;
    overflow: visible;
  }
  .grid {
    stroke: var(--border-hairline);
    stroke-dasharray: 2 4;
    stroke-width: 1;
  }
  .crosshair {
    stroke: var(--border-strong);
    stroke-dasharray: 3 3;
    stroke-width: 1;
  }
  /* The series and its area draw in from the left when the chart renders. */
  .plot {
    animation: draw-x var(--dur-slow) var(--ease-out) backwards;
  }
  .area-top {
    stop-color: var(--chart);
    stop-opacity: 0.28;
  }
  .area-bottom {
    stop-color: var(--chart);
    stop-opacity: 0;
  }
  .series {
    fill: none;
    stroke: var(--chart);
    stroke-width: 1.5;
    stroke-linejoin: round;
    stroke-linecap: round;
  }
  .point {
    fill: var(--canvas);
    stroke: var(--chart);
    stroke-width: 1.5;
  }
  .point.active {
    fill: var(--chart);
    stroke: var(--foreground-hover);
  }
  .axis-label {
    fill: var(--foreground-subtle);
    font-family: var(--font-mono);
    font-size: var(--text-caption);
    letter-spacing: -0.02em;
    text-transform: uppercase;
  }
  @media (max-width: 36rem) {
    .usage-chart {
      padding: 1rem;
    }
    .chart {
      height: 15rem;
    }
    figcaption {
      display: grid;
    }
  }
  @media (forced-colors: active) {
    .legend span {
      background: CanvasText;
    }
    .area {
      display: none;
    }
  }
</style>
