<script lang="ts">
  import { notify } from '#lib/components/toasts.svelte.js';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { useAction } from '#lib/editing/action.svelte.js';
  import { useRole } from '#lib/session/useRole.svelte.js';
  import { errorMessage } from '#lib/management/http.js';
  import { getExposure, putExposure } from './api';
  import { catalogKeys } from './catalogKeys';
  let { routeId }: { routeId: string } = $props();
  const access = useRole();
  const client = useQueryClient();
  const exposure = createQuery(() => ({
    queryKey: catalogKeys.exposure(routeId),
    queryFn: ({ signal }) => getExposure(routeId, signal)
  }));
  let exposed = $state(false);
  let etag = $state('');
  const action = useAction();
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
      action.error = '';
    }
  }
  async function save(event: SubmitEvent) {
    event.preventDefault();
    await action.run(
      () => putExposure(routeId, { expose_upstream_models: exposed, etag }),
      async (result) => {
        client.setQueryData(catalogKeys.exposure(routeId), result);
        await client.invalidateQueries({ queryKey: catalogKeys.root });
        etag = result.etag;
        notify('Catalog disclosure saved.');
      }
    );
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
  {#if action.error}<p role="alert" class="inline-problem">
      {action.error}
    </p>{/if}

  <form onsubmit={save}>
    <label>
      <input
        type="checkbox"
        bind:checked={exposed}
        disabled={action.busy ||
          !etag ||
          !access.allows('PUT /api/v1/routes/{route_id}/catalog')}
      /> Disclose upstream model names in authenticated and published catalogs
    </label>
    <div class="actions">
      <button
        class="button button-primary"
        disabled={action.busy ||
          !etag ||
          !access.allows('PUT /api/v1/routes/{route_id}/catalog')}
      >
        Save disclosure
      </button><button
        class="button button-secondary"
        type="button"
        disabled={action.busy || exposure.isFetching}
        onclick={reload}
      >
        Reload disclosure
      </button>
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
