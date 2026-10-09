<script lang="ts">
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { useRole } from '$lib/features/access/session/useRole.svelte';
  import { errorMessage } from '$lib/api/http';
  import { catalogKeys, getExposure, putExposure } from './api';
  let { routeId }: { routeId: string } = $props();
  const access = useRole();
  const client = useQueryClient();
  const exposure = createQuery(() => ({
    queryKey: catalogKeys.exposure(routeId),
    queryFn: ({ signal }) => getExposure(routeId, signal)
  }));
  let exposed = $state(false);
  let etag = $state('');
  let busy = $state(false);
  let error = $state('');
  let notice = $state('');
  $effect(() => {
    if (exposure.data && !etag) {
      exposed = exposure.data.expose_upstream_models;
      etag = exposure.data.etag;
    }
  });
  async function reload() {
    const result = await exposure.refetch();
    if (result.data) {
      exposed = result.data.expose_upstream_models;
      etag = result.data.etag;
      error = notice = '';
    }
  }
  async function save(event: SubmitEvent) {
    event.preventDefault();
    busy = true;
    error = notice = '';
    try {
      const result = await putExposure(routeId, {
        expose_upstream_models: exposed,
        etag
      });
      client.setQueryData(catalogKeys.exposure(routeId), result);
      await client.invalidateQueries({ queryKey: catalogKeys.root });
      etag = result.etag;
      notice = 'Catalog disclosure saved.';
    } catch (cause) {
      error = errorMessage(cause);
    } finally {
      busy = false;
    }
  }
</script>

<section class="card" aria-label="Catalog disclosure">
  <h2>Catalog disclosure</h2>
  <p>
    Catalogs show the public route name by default. Provider identities and
    endpoints remain private.
  </p>
  {#if exposure.isError}<p role="alert" class="inline-problem">
      {errorMessage(exposure.error)}
    </p>{/if}
  {#if error}<p role="alert" class="inline-problem">{error}</p>{/if}
  {#if notice}<p role="status">{notice}</p>{/if}
  <form onsubmit={save}>
    <label
      ><input
        type="checkbox"
        bind:checked={exposed}
        disabled={busy ||
          !etag ||
          !access.allows('PUT /api/v1/routes/{route_id}/catalog')}
      /> Disclose upstream model names in authenticated and published catalogs</label
    >
    <div class="actions">
      <button
        class="button button-primary"
        disabled={busy ||
          !etag ||
          !access.allows('PUT /api/v1/routes/{route_id}/catalog')}
        >Save disclosure</button
      ><button
        class="button button-secondary"
        type="button"
        disabled={busy || exposure.isFetching}
        onclick={reload}>Reload disclosure</button
      >
    </div>
  </form>
</section>

<style>
  section {
    padding: 1.5rem;
  }
  h2 {
    font-size: 1.125rem;
    font-weight: 600;
  }
  p {
    margin: 0.75rem 0;
    line-height: 1.6;
    max-width: 70ch;
  }
  label {
    display: flex;
    align-items: start;
    gap: 0.5rem;
    margin: 1rem 0;
  }
  .actions {
    display: flex;
    flex-wrap: wrap;
    gap: 0.75rem;
  }
</style>
