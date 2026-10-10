<script lang="ts">
  import { createQuery } from '@tanstack/svelte-query';
  import { listExposureRoutes } from './api';
  import { errorMessage } from '$lib/api/http';
  import ExposurePanel from './ExposurePanel.svelte';
  let open = $state(false);
  let selected = $state('');
  const routes = createQuery(() => ({
    queryKey: ['model-catalog', 'exposure-routes'],
    queryFn: ({ signal }) => listExposureRoutes(signal),
    enabled: open
  }));
</script>

<details bind:open>
  <summary>Catalog disclosure settings</summary>
  {#if routes.isError}<p role="alert">{errorMessage(routes.error)}</p>
    <button class="button button-secondary" onclick={() => routes.refetch()}
      >Reload published routes</button
    >{/if}
  <label
    >Published route<select
      aria-label="Published route"
      class="filter-control"
      bind:value={selected}
      ><option value="">Choose a published route</option
      >{#each routes.data ?? [] as route (route.id)}<option value={route.id}
          >{route.slug}</option
        >{/each}</select
    ></label
  >
  {#if selected}{#key selected}<ExposurePanel routeId={selected} />{/key}{/if}
</details>

<style>
  details {
    margin: 1.5rem 0;
  }
  summary {
    cursor: pointer;
    font-weight: 600;
  }
  label {
    display: grid;
    gap: 0.5rem;
    max-width: 28rem;
    margin: 1rem 0;
  }
</style>
