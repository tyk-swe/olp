<script lang="ts">
  import { createQuery } from '@tanstack/svelte-query';
  import { resolve } from '$app/paths';
  import ScrollTable from '#lib/components/ScrollTable.svelte';
  import { dateTimeLocalValue, formatInteger } from '#lib/format.js';
  import { errorMessage } from '#lib/management/http.js';
  import { useRole } from '#lib/features/access/session/useRole.svelte.js';
  import { defaultUsageState } from './usageState';
  import {
    selectorSavings,
    shadowExperiments,
    type RoutingReportFilters
  } from './usage';
  import { usageKeys } from './usageKeys';
  const measurements = [
    ['Successful requests', 'successes'],
    ['Mean latency in milliseconds', 'mean_latency_ms'],
    ['Mean first byte in milliseconds', 'mean_first_byte_ms'],
    ['Input tokens', 'input_tokens'],
    ['Output tokens', 'output_tokens'],
    ['Estimated cost', 'estimated_cost'],
    ['Unpriced requests', 'unpriced_requests']
  ] as const;
  const access = useRole();
  const initial = defaultUsageState().filters;
  let start = $state(dateTimeLocalValue(initial.start));
  let end = $state(dateTimeLocalValue(initial.end));
  let route = $state('');
  let project = $state('');
  let error = $state('');
  let applied = $state<RoutingReportFilters>({
    start: initial.start,
    end: initial.end
  });
  const savings = createQuery(() => ({
    queryKey: usageKeys.routing('savings', applied),
    queryFn: ({ signal }) => selectorSavings(applied, signal),
    enabled: access.allows('GET /api/v1/usage/selector-savings')
  }));
  const shadows = createQuery(() => ({
    queryKey: usageKeys.routing('shadows', applied),
    queryFn: ({ signal }) => shadowExperiments(applied, signal),
    enabled: access.allows('GET /api/v1/usage/shadow-experiments')
  }));
  function apply(event: SubmitEvent) {
    event.preventDefault();
    const first = new Date(start);
    const last = new Date(end);
    if (
      !Number.isFinite(first.getTime()) ||
      !Number.isFinite(last.getTime()) ||
      first >= last
    ) {
      error = 'Choose an end time after the start';
      return;
    }
    error = '';
    applied = {
      start: first.toISOString(),
      end: last.toISOString(),
      route: route || undefined,
      project_id: project || undefined
    };
  }
</script>

<svelte:head><title>Routing reports · OpenLLMProxy</title></svelte:head>
<div class="page-header">
  <div>
    <p class="eyebrow">Operations</p>
    <h1 class="page-title">Routing reports</h1>
    <p class="page-description">
      Selector savings and shadow comparisons for the chosen interval.
    </p>
  </div>
</div>
<nav class="page-tabs" aria-label="Usage reports">
  <a href={resolve('usage')}>Usage overview</a>

  <a aria-current="page" href={resolve('usage/routing')}>Routing reports</a>
</nav>
<form class="card form-card" onsubmit={apply}>
  <label class="form-field"
    >Start<input required type="datetime-local" bind:value={start} /></label
  >
  <label class="form-field"
    >End<input required type="datetime-local" bind:value={end} /></label
  >
  <label class="form-field">Route<input bind:value={route} /></label>
  <label class="form-field">Project ID<input bind:value={project} /></label>
  <div class="page-actions">
    <button class="button button-primary">Apply filters</button>
  </div>
</form>
{#if error}<p class="inline-problem" role="alert">{error}</p>{/if}
{#if savings.error || shadows.error}
  <p class="inline-problem" role="alert">
    {errorMessage(savings.error ?? shadows.error)}
  </p>
{/if}
{#if savings.isFetching || shadows.isFetching}
  <p class="inline-status" role="status">Loading reports…</p>
{/if}
{#if savings.data}
  <section class="section" aria-labelledby="selector-savings">
    <h2 id="selector-savings">Selector savings</h2>
    <p class="inline-status">
      Currency {savings.data.currency ?? 'Unavailable'}. Compared attempts have
      prices for both the selected and avoided targets.
    </p>
    {#if !savings.data.items.length}
      <div class="card empty-state">
        No selector comparisons in this interval
      </div>
    {:else}
      <ScrollTable label="Selector savings">
        <table class="data-table">
          <caption class="sr-only">Selector savings by route</caption>
          <thead>
            <tr>
              <th scope="col">Route</th>
              <th scope="col">Selector</th>
              <th scope="col">Compared / attempts</th>
              <th scope="col">Estimated cost</th>
              <th scope="col">Baseline cost</th>
              <th scope="col">Savings</th>
            </tr>
          </thead>
          <tbody>
            {#each savings.data.items as value (`${value.route}:${value.selector}`)}
              <tr>
                <td>{value.route}</td>
                <td>{value.selector}</td>
                <td
                  >{formatInteger(value.compared_attempts)} / {formatInteger(
                    value.attempts
                  )}</td
                >
                <td
                  >{value.compared_attempts
                    ? value.estimated_cost
                    : 'Unavailable'}</td
                >
                <td
                  >{value.compared_attempts
                    ? value.baseline_cost
                    : 'Unavailable'}</td
                >
                <td
                  >{value.compared_attempts ? value.savings : 'Unavailable'}</td
                >
              </tr>
            {/each}
          </tbody>
        </table>
      </ScrollTable>
    {/if}
  </section>
{/if}
{#if shadows.data}
  <section class="section" aria-labelledby="shadow-comparisons">
    <h2 id="shadow-comparisons">Shadow comparisons</h2>
    <p class="inline-status">
      Currency {shadows.data.currency ?? 'Unavailable'}
    </p>
    {#if !shadows.data.items.length}
      <div class="card empty-state">No shadow comparisons in this interval</div>
    {/if}
    {#each shadows.data.items as value (`${value.route}:${value.provider_id}:${value.upstream_model}`)}
      <article class="card stack-card">
        <h3>{value.route} · {value.upstream_model}</h3>
        <p class="inline-status">
          {formatInteger(value.pairs)} mirrored pairs · {value.provider_id}
        </p>
        <ScrollTable label={`Shadow comparison for ${value.route}`}>
          <table class="data-table">
            <caption class="sr-only"
              >Primary and shadow measurements for {value.route}</caption
            >
            <thead>
              <tr>
                <th scope="col">Measurement</th>
                <th scope="col">Primary</th>
                <th scope="col">Shadow</th>
              </tr>
            </thead>
            <tbody>
              {#each measurements as [label, key] (key)}
                <tr>
                  <th scope="row">{label}</th>
                  <td>{value.primary[key] ?? 'Unavailable'}</td>
                  <td>{value.shadow[key] ?? 'Unavailable'}</td>
                </tr>
              {/each}
            </tbody>
          </table>
        </ScrollTable>
      </article>
    {/each}
  </section>
{/if}
