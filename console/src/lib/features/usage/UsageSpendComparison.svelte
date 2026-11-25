<script lang="ts">
  import { createQuery } from '@tanstack/svelte-query';
  import {
    listProjectMemberships,
    projectKeys
  } from '#lib/features/access/index.js';
  import ScrollTable from '#lib/components/ScrollTable.svelte';
  import { formatBudget, formatCost } from '#lib/format.js';
  import { errorMessage } from '#lib/management/http.js';
  import { presentUsageCompleteness } from './completenessPresentation.js';
  import {
    usageComparison,
    type UsageComparisonFilters,
    type UsageFilters
  } from './usage.js';
  import { usageKeys } from './usageKeys.js';

  let { filters }: { filters: UsageFilters } = $props();

  const day = 86_400_000;
  const midnight = (date: string) => `${date}T00:00:00.000Z`;
  const today = () => new Date().toISOString().slice(0, 10);
  const daysBefore = (end: string, days: number) =>
    new Date(Date.parse(midnight(end)) - days * day).toISOString().slice(0, 10);

  function validRange(start: string, end: string) {
    const days =
      (Date.parse(midnight(end)) - Date.parse(midnight(start))) / day;
    return Number.isInteger(days) && days >= 1 && days <= 31 && end <= today();
  }

  function initialDates(current: UsageFilters) {
    const start = current.start.slice(0, 10);
    const end = current.end.slice(0, 10);
    const lastClosedEnd = today();
    const isMidnight = (value: string) => /T00:00:00(?:\.0+)?Z$/.test(value);
    return isMidnight(current.start) &&
      isMidnight(current.end) &&
      validRange(start, end)
      ? { start, end }
      : { start: daysBefore(lastClosedEnd, 7), end: lastClosedEnd };
  }

  let draft = $state({ start: '', end: '', project: '' });
  let applied = $state<UsageComparisonFilters | null>(null);
  let validation = $state('');

  $effect(() => {
    draft = { ...initialDates(filters), project: filters.project_id ?? '' };
    applied = null;
    validation = '';
  });

  const carriedFilters = $derived(
    Object.entries(filters).filter(
      ([key, value]) =>
        !['start', 'end', 'project_id'].includes(key) && Boolean(value)
    )
  );
  const projects = createQuery(() => ({
    queryKey: projectKeys.memberships,
    queryFn: ({ signal }) => listProjectMemberships(signal)
  }));
  const comparison = createQuery(() => {
    const selected = applied;
    return {
      queryKey: usageKeys.comparison(selected),
      enabled: selected !== null,
      queryFn: ({ signal }) =>
        selected ? usageComparison(selected, signal) : null
    };
  });

  function compare(event: SubmitEvent) {
    event.preventDefault();
    if (!draft.project || !validRange(draft.start, draft.end)) {
      validation = 'Select one project and 1 to 31 closed UTC days.';
      return;
    }
    validation = '';
    const start = midnight(draft.start);
    const end = midnight(draft.end);
    if (
      applied?.project_id === draft.project &&
      applied.start === start &&
      applied.end === end
    ) {
      void comparison.refetch();
      return;
    }
    applied = {
      ...filters,
      project_id: draft.project,
      start,
      end
    };
  }

  function amount(value: string, currency: string | null) {
    return value === '0'
      ? formatCost(value, currency)
      : formatBudget(value, currency);
  }

  function signedAmount(value: string, currency: string | null) {
    if (value === '0') return amount(value, currency);
    if (value.startsWith('-')) return `−${amount(value.slice(1), currency)}`;
    return `+${amount(value, currency)}`;
  }

  function signedPercent(value: string | null) {
    if (value === null) return 'Not applicable';
    if (value === '0') return '0%';
    return value.startsWith('-') ? `−${value.slice(1)}%` : `+${value}%`;
  }
</script>

<section class="comparison" aria-labelledby="usage-comparison-title">
  <h2 id="usage-comparison-title">Compare estimated cost by route</h2>
  <p>
    The previous period is the same length and ends when the current period
    starts. Dates are UTC, with an inclusive start and an exclusive end.
  </p>
  <form class="filter-bar" onsubmit={compare}>
    <label class="form-field">
      Project
      <select bind:value={draft.project} required>
        <option value="">Select a project</option>
        {#each projects.data ?? [] as project (project.id)}
          <option value={project.id}>{project.name}</option>
        {/each}
      </select>
    </label>
    <label class="form-field">
      From (UTC)
      <input type="date" bind:value={draft.start} required />
    </label>
    <label class="form-field">
      To (UTC, exclusive)
      <input type="date" bind:value={draft.end} max={today()} required />
    </label>
    <button
      class="button button-primary"
      type="submit"
      disabled={comparison.isFetching || projects.isPending}
    >
      Compare
    </button>
  </form>
  {#if projects.isError}
    <p class="inline-problem" role="alert">
      {errorMessage(projects.error, 'Projects could not be loaded.')}
    </p>
  {:else if projects.data?.length === 0}
    <p>No projects are available to compare.</p>
  {/if}
  {#if carriedFilters.length}
    <p>
      Filters from Usage apply to both periods. Turn comparison off to edit
      them.
    </p>
    <dl class="detail-list">
      {#each carriedFilters as [key, value] (key)}
        <dt>{key.replaceAll('_', ' ')}</dt>
        <dd>{value}</dd>
      {/each}
    </dl>
  {/if}
  {#if validation}
    <p class="inline-problem" role="alert">{validation}</p>
  {/if}

  {#if applied && comparison.isPending}
    <p class="loading-state" role="status">Comparing usage…</p>
  {:else if comparison.isError}
    <p class="inline-problem" role="alert">
      {errorMessage(comparison.error, 'The comparison could not be loaded.')}
      <button
        class="text-button"
        type="button"
        onclick={() => comparison.refetch()}
      >
        Try again
      </button>
    </p>
  {:else if comparison.data}
    {@const report = comparison.data}
    {@const project = projects.data?.find(
      ({ id }) => id === applied?.project_id
    )}
    <p>
      Comparing estimated cost for <strong>
        {project?.name ?? applied?.project_id}
      </strong>
    </p>
    <div class="periods">
      {#each [{ title: 'Previous period', period: report.previous }, { title: 'Current period', period: report.current }] as { title, period } (title)}
        {@const evidence = presentUsageCompleteness(period.completeness)}
        <article class="card period">
          <h3>{title}</h3>
          <p>
            {period.start.slice(0, 10)} to {period.end.slice(0, 10)} UTC (end excluded)
          </p>
          {#if evidence.kind === 'complete'}
            <span class="badge success">Accounting complete</span>
          {:else}
            <p class="badge warning">{evidence.title}</p>
            <p>{evidence.detail}</p>
          {/if}
          <h4>Observed estimated cost</h4>
          {#each period.subtotals as subtotal (subtotal.currency)}
            <p>
              {amount(subtotal.estimated_cost, subtotal.currency)} ({subtotal.currency ??
                'unknown currency'})
            </p>
          {:else}
            {#if report.status === 'available' && report.currency}
              <p>{amount('0', report.currency)} ({report.currency})</p>
            {:else}
              <p>No priced usage recorded.</p>
            {/if}
          {/each}
        </article>
      {/each}
    </div>

    {#if report.status === 'available' && report.currency}
      <h3>Largest route spend changes ({report.currency})</h3>
      <ScrollTable label="Route spend changes">
        <table class="data-table">
          <caption class="sr-only">
            Up to 25 routes ranked by absolute estimated cost change
          </caption>
          <thead>
            <tr>
              <th scope="col">Route</th>
              <th scope="col">Previous cost</th>
              <th scope="col">Current cost</th>
              <th scope="col">Change</th>
              <th scope="col">Percent change</th>
            </tr>
          </thead>
          <tbody>
            {#each report.items as row (row.route)}
              <tr>
                <th scope="row">{row.route}</th>
                <td>{amount(row.previous_cost, report.currency)}</td>
                <td>{amount(row.current_cost, report.currency)}</td>
                <td>{signedAmount(row.change, report.currency)}</td>
                <td>{signedPercent(row.percent_change)}</td>
              </tr>
            {/each}
          </tbody>
        </table>
      </ScrollTable>
    {:else}
      <p role="status">
        <strong>Comparison unavailable.</strong>
        {#if report.status === 'incomplete'}
          Accounting or pricing is incomplete for at least one period.
        {:else if report.status === 'incompatible_currency'}
          The observed costs do not share one known currency.
        {:else}
          Neither period has recorded priced usage.
        {/if}
      </p>
    {/if}
  {/if}
</section>

<style>
  .comparison {
    margin-top: 1.5rem;
  }
  .periods {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(min(100%, 18rem), 1fr));
    gap: 1rem;
    margin-block: 1.5rem;
  }
  .period {
    padding: 1rem;
  }
  .period h3 {
    margin-top: 0;
  }
</style>
