<script lang="ts">
  import { resolve } from '$app/paths';
  import { createQuery } from '@tanstack/svelte-query';
  import { errorMessage } from '$lib/api/http';
  import { getInvestigation } from './api';
  import { agentKeys } from './agentKeys';

  let { investigationID }: { investigationID: string } = $props();
  const investigation = createQuery(() => ({
    queryKey: agentKeys.investigation(investigationID),
    queryFn: () => getInvestigation(investigationID),
    staleTime: 0,
    gcTime: 0,
    retry: false
  }));
</script>

<svelte:head><title>Investigation · OpenLLMProxy</title></svelte:head>
<a class="text-button" href={resolve('/investigations')}>All investigations</a>
<header>
  <p class="eyebrow">Gateway</p>
  <h1>Investigation</h1>
</header>
{#if investigation.error}
  <p role="alert">{errorMessage(investigation.error)}</p>
{:else if investigation.isFetching}
  <p>Loading investigation…</p>
{:else if investigation.data}
  {@const item = investigation.data}
  <dl>
    <dt>Project</dt>
    <dd><code>{item.project_id}</code></dd>
    <dt>Saved</dt>
    <dd>{new Date(item.created_at).toLocaleString()}</dd>
    <dt>Summary</dt>
    <dd class="summary">{item.summary}</dd>
  </dl>
  <section aria-labelledby="filters-heading">
    <h2 id="filters-heading">Filters</h2>
    <dl>
      <dt>From</dt>
      <dd>
        {item.filters.from
          ? new Date(item.filters.from).toLocaleString()
          : 'Any'}
      </dd>
      <dt>To</dt>
      <dd>
        {item.filters.to ? new Date(item.filters.to).toLocaleString() : 'Any'}
      </dd>
      <dt>Run</dt>
      <dd>
        {#if item.filters.run_id}<a
            class="text-button"
            href={resolve(`/runs/${item.filters.run_id}`)}
            ><code>{item.filters.run_id}</code></a
          >{:else}Any{/if}
      </dd>
      <dt>Minimum duration</dt>
      <dd>
        {item.filters.min_duration_ms === undefined
          ? 'Any'
          : `${item.filters.min_duration_ms} ms`}
      </dd>
      <dt>Maximum duration</dt>
      <dd>
        {item.filters.max_duration_ms === undefined
          ? 'Any'
          : `${item.filters.max_duration_ms} ms`}
      </dd>
      <dt>Error</dt>
      <dd>
        {item.filters.error === undefined
          ? 'Any'
          : item.filters.error
            ? 'Error'
            : 'No error'}
      </dd>
      <dt>Tool</dt>
      <dd>{item.filters.tool ?? 'Any'}</dd>
    </dl>
  </section>
  <section aria-labelledby="evidence-heading">
    <h2 id="evidence-heading">Evidence references</h2>
    <p>
      Evidence is accessed separately and may expire under its own retention
      policy.
    </p>
    {#if !item.evidence.length}
      <p>No references saved.</p>
    {:else}
      <ul>
        {#each item.evidence as reference, index (index)}
          <li>
            {reference.kind}
            {#if reference.kind === 'run'}
              <a class="text-button" href={resolve(`/runs/${reference.id}`)}
                ><code>{reference.id}</code></a
              >
            {:else if reference.kind === 'task'}
              <a
                class="text-button"
                href={resolve(`/a2a-tasks/${reference.id}`)}
                ><code>{reference.id}</code></a
              >
            {:else if reference.kind === 'delegation'}
              <a
                class="text-button"
                href={`${resolve('/delegations')}?project_id=${encodeURIComponent(item.project_id)}&delegation_id=${encodeURIComponent(reference.id)}`}
                ><code>{reference.id}</code></a
              >
            {:else if reference.run_id}
              <a
                class="text-button"
                href={reference.kind === 'step' || reference.kind === 'span'
                  ? `${resolve(`/runs/${reference.run_id}`)}?${reference.kind}_id=${encodeURIComponent(reference.id)}`
                  : resolve(`/runs/${reference.run_id}`)}
                ><code>{reference.id}</code></a
              >
            {:else}
              <code>{reference.id}</code>
            {/if}
          </li>
        {/each}
      </ul>
      <p>Links to steps, approvals and spans open their associated run.</p>
    {/if}
  </section>
{/if}

<style>
  header,
  section {
    margin: 1rem 0;
  }
  dt {
    font-weight: 600;
    margin-top: 0.75rem;
  }
  dd {
    margin: 0.25rem 0;
  }
  .summary {
    white-space: pre-wrap;
  }
  code {
    overflow-wrap: anywhere;
  }
  li {
    margin: 0.5rem 0;
  }
</style>
