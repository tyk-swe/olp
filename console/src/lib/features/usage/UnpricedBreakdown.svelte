<script lang="ts">
  import { resolve } from '$app/paths';
  import { createQuery } from '@tanstack/svelte-query';
  import ScrollTable from '#lib/components/ScrollTable.svelte';
  import { formatInteger } from '#lib/format.js';
  import { errorMessage } from '#lib/management/http.js';
  import { useRole } from '#lib/session/useRole.svelte.js';
  import type { components } from '#lib/management/schema.js';
  import { unpricedBreakdown, type UsageFilters } from './usage.js';
  import { usageKeys } from './usageKeys.js';
  import { providerQueries } from '#lib/domain/providers/queries.js';

  let { filters }: { filters: UsageFilters } = $props();
  const access = useRole();
  const report = createQuery(() => ({
    queryKey: usageKeys.unpriced(filters),
    queryFn: ({ signal, queryKey }) => unpricedBreakdown(queryKey[2], signal)
  }));
  const providers = createQuery(() => ({
    ...providerQueries.all(),
    enabled:
      Boolean(report.data?.items.length) &&
      access.allows('GET /api/v1/providers')
  }));
  const providerNames = $derived(
    Object.fromEntries((providers.data ?? []).map(({ id, name }) => [id, name]))
  );
  const causes: Record<
    components['schemas']['UnpricedBreakdownItem']['cause'],
    string
  > = {
    no_applicable_price: 'No applicable price',
    missing_price_component: 'Missing price component',
    incomplete_usage: 'Incomplete upstream usage',
    not_recorded: 'Cause not recorded'
  };
</script>

<section aria-labelledby="unpriced-breakdown-title">
  <header>
    <h2 id="unpriced-breakdown-title">Unpriced attempts</h2>
    <p>
      Top groups by affected attempt count. Retained totals cover whole hours
      only.
    </p>
  </header>
  {#if report.isPending}
    <p role="status">Loading unpriced attempts…</p>
  {:else if report.isError}
    <p role="alert">{errorMessage(report.error)}</p>
  {:else if report.data.items.length === 0}
    <p>No unpriced attempts in this time range.</p>
  {:else}
    <ScrollTable
      label="Unpriced attempts by provider, model, operation and cause"
    >
      <table class="data-table">
        <thead>
          <tr>
            <th scope="col">Provider</th>
            <th scope="col">Model</th>
            <th scope="col">Operation</th>
            <th scope="col">Cause</th>
            <th scope="col" aria-sort="descending">Attempts</th>
            <th scope="col">Inspect</th>
          </tr>
        </thead>
        <tbody>
          {#each report.data.items as row (`${row.provider_id}:${row.operation}:${row.cause}:${row.model}`)}
            <tr>
              <td>{providerNames[row.provider_id] ?? row.provider_id}</td>
              <td>{row.model}</td>
              <td>{row.operation}</td>
              <td>{causes[row.cause]}</td>
              <td>{formatInteger(row.attempt_count)}</td>
              <td>
                {#if row.sample_request_id && access.allows('GET /api/v1/requests/{request_id}')}
                  <a href={resolve(`requests/${row.sample_request_id}`)}>
                    View request
                  </a>
                {:else if (row.cause === 'no_applicable_price' || row.cause === 'missing_price_component') && access.globalScope && access.allows('GET /api/v1/pricing/revisions')}
                  <a href={resolve('pricing/revisions')}>View pricing</a>
                {:else}
                  Unavailable
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
  section {
    display: grid;
    gap: 1rem;
  }

  p {
    color: var(--foreground-muted);
  }
</style>
