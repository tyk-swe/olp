<script lang="ts">
  import ScrollTable from '#lib/components/ScrollTable.svelte';
  import type { UsagePoint } from '#lib/features/usage/usage.js';
  import { formatCost } from '#lib/format.js';

  let {
    points,
    bucketLabel
  }: {
    points: UsagePoint[];
    bucketLabel: (value: string | null | undefined) => string;
  } = $props();
</script>

<details>
  <summary>View chart data</summary>
  <ScrollTable label="Chart data table">
    <table class="data-table">
      <caption class="sr-only">
        Exact usage values shown in the time-series chart
      </caption>
      <thead>
        <tr>
          <th scope="col">Bucket</th>
          <th scope="col" class="num">Requests</th>
          <th scope="col" class="num">Input tokens</th>
          <th scope="col" class="num">Cached input</th>
          <th scope="col" class="num">Cache write</th>
          <th scope="col" class="num">Output tokens</th>
          <th scope="col" class="num">Estimated cost</th>
          <th scope="col">Status</th>
        </tr>
      </thead>
      <tbody>
        {#each points as point (point.bucket)}
          <tr>
            <td>{bucketLabel(point.bucket)}</td>
            <td class="num">{point.request_count}</td>
            <td class="num">{point.input_tokens}</td>
            <td class="num">{point.cached_input_tokens ?? '—'}</td>
            <td class="num">{point.cache_write_input_tokens ?? '—'}</td>
            <td class="num">{point.output_tokens}</td>
            <td class="num">
              {formatCost(point.estimated_cost, point.currency)}
            </td>
            <td>
              {point.incomplete_count > 0
                ? `${point.incomplete_count} incomplete`
                : point.unpriced_count > 0
                  ? `${point.unpriced_count} unpriced`
                  : 'Complete'}
            </td>
          </tr>
        {/each}
      </tbody>
    </table>
  </ScrollTable>
</details>

<style>
  details {
    margin-top: 0.5rem;
  }
  summary {
    display: inline-flex;
    min-height: 2.75rem;
    align-items: center;
    color: var(--foreground);
    font-weight: 400;
    text-decoration: underline;
    text-decoration-color: var(--border-strong);
    text-underline-offset: 4px;
    transition: text-decoration-color var(--motion);
    cursor: pointer;
  }
  summary:hover {
    text-decoration-color: currentColor;
  }
</style>
