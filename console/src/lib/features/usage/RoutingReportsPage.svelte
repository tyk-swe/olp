<script lang="ts">
  import { createQuery } from '@tanstack/svelte-query';
  import { resolve } from '$app/paths';
  import { dateTimeLocalValue } from '$lib/format';
  import { errorMessage } from '$lib/management/http';
  import { useRole } from '$lib/features/access/session/useRole.svelte';
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
<section class="workflow">
  <h1 class="page-title">Routing reports</h1>
  <nav aria-label="Usage reports">
    <a href={resolve('/usage')}>Usage overview</a><a
      aria-current="page"
      href={resolve('/usage/routing')}>Routing reports</a
    >
  </nav>
  <form class="card" onsubmit={apply}>
    <label
      >Start<input required type="datetime-local" bind:value={start} /></label
    ><label>End<input required type="datetime-local" bind:value={end} /></label>
    <label>Route<input bind:value={route} /></label><label
      >Project ID<input bind:value={project} /></label
    ><button class="button button-primary">Apply filters</button>
  </form>
  {#if error}<p role="alert">{error}</p>{/if}
  {#if savings.error || shadows.error}<p role="alert">
      {errorMessage(savings.error ?? shadows.error)}
    </p>{/if}
  {#if savings.isFetching || shadows.isFetching}<p role="status">
      Loading reports
    </p>{/if}
  {#if savings.data}
    <section class="card">
      <h2>Selector savings</h2>
      <p>
        Currency {savings.data.currency ?? 'Unavailable'}. Compared attempts
        have prices for both the selected and avoided targets
      </p>
      <table>
        <thead
          ><tr
            ><th>Route</th><th>Selector</th><th>Compared / attempts</th><th
              >Estimated cost</th
            ><th>Baseline cost</th><th>Savings</th></tr
          ></thead
        ><tbody>
          {#each savings.data.items as value, index (index)}<tr
              ><td>{value.route}</td><td>{value.selector}</td><td
                >{value.compared_attempts} / {value.attempts}</td
              ><td
                >{value.compared_attempts
                  ? value.estimated_cost
                  : 'Unavailable'}</td
              ><td
                >{value.compared_attempts
                  ? value.baseline_cost
                  : 'Unavailable'}</td
              ><td>{value.compared_attempts ? value.savings : 'Unavailable'}</td
              ></tr
            >{/each}
        </tbody>
      </table>
      {#if !savings.data.items.length}<p>
          No selector comparisons in this interval
        </p>{/if}
    </section>
  {/if}
  {#if shadows.data}<section>
      <h2>Shadow comparisons</h2>
      <p>Currency {shadows.data.currency ?? 'Unavailable'}</p>
      {#each shadows.data.items as value, index (index)}<article class="card">
          <h3>{value.route} · {value.upstream_model}</h3>
          <p>{value.pairs} mirrored pairs · {value.provider_id}</p>
          <table>
            <thead
              ><tr><th>Measurement</th><th>Primary</th><th>Shadow</th></tr
              ></thead
            ><tbody>
              {#each measurements as [label, key] (key)}
                <tr
                  ><th>{label}</th><td>{value.primary[key] ?? 'Unavailable'}</td
                  ><td>{value.shadow[key] ?? 'Unavailable'}</td></tr
                >
              {/each}</tbody
            >
          </table>
        </article>{/each}
      {#if !shadows.data.items.length}<p>
          No shadow comparisons in this interval
        </p>{/if}
    </section>{/if}
</section>
