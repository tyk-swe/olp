<script lang="ts">
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { useRole } from '$lib/features/access/session/useRole.svelte';
  import { errorMessage } from '$lib/api/http';
  import { catalogKeys, getPublication, putPublication } from './api';
  let { projectId }: { projectId: string } = $props();
  const access = useRole();
  const client = useQueryClient();
  const publication = createQuery(() => ({
    queryKey: catalogKeys.publication(projectId),
    queryFn: ({ signal }) => getPublication(projectId, signal)
  }));
  let enabled = $state(false);
  let pricesPublic = $state(false);
  let etag = $state('');
  let busy = $state(false);
  let error = $state('');
  let notice = $state('');
  $effect(() => {
    if (publication.data && !etag) {
      enabled = publication.data.enabled;
      pricesPublic = publication.data.prices_public;
      etag = publication.data.etag;
    }
  });
  async function reload() {
    const result = await publication.refetch();
    if (result.data) {
      enabled = result.data.enabled;
      pricesPublic = result.data.prices_public;
      etag = result.data.etag;
      error = notice = '';
    }
  }
  async function save(event: SubmitEvent) {
    event.preventDefault();
    busy = true;
    error = notice = '';
    try {
      const result = await putPublication(projectId, {
        enabled,
        prices_public: pricesPublic,
        etag
      });
      client.setQueryData(catalogKeys.publication(projectId), result);
      etag = result.etag;
      notice = 'Catalog publication saved.';
    } catch (cause) {
      error = errorMessage(cause);
    } finally {
      busy = false;
    }
  }
</script>

<section class="card" aria-label="Public model catalog">
  <h2>Public model catalog</h2>
  <p>
    Share this project's published routes, capabilities, privacy declarations
    and SDK examples without requiring a login. Prices need a separate opt-in.
  </p>
  {#if publication.isError}<p role="alert" class="inline-problem">
      {errorMessage(publication.error)}
    </p>{/if}
  {#if error}<p role="alert" class="inline-problem">{error}</p>{/if}
  {#if notice}<p role="status">{notice}</p>{/if}
  {#if !access.allows('PUT /api/v1/projects/{project_id}/catalog')}<p
      class="muted"
    >
      An installation owner can change public catalog publication.
    </p>{/if}
  <form onsubmit={save}>
    <fieldset
      disabled={busy ||
        !etag ||
        !access.allows('PUT /api/v1/projects/{project_id}/catalog')}
    >
      <label
        ><input type="checkbox" bind:checked={enabled} /> Publish project catalog</label
      >
      <label
        ><input type="checkbox" bind:checked={pricesPublic} /> Include current declared
        prices in the public catalog</label
      >
    </fieldset>
    <div class="actions">
      <button
        class="button button-primary"
        disabled={busy ||
          !etag ||
          !access.allows('PUT /api/v1/projects/{project_id}/catalog')}
        >Save publication</button
      ><button
        class="button button-secondary"
        type="button"
        disabled={busy || publication.isFetching}
        onclick={reload}>Reload publication</button
      >
    </div>
  </form>
  {#if publication.data?.enabled}<p>
      <a href={`/catalog/public/${projectId}`} target="_blank" rel="noreferrer"
        >Open public model catalog</a
      >
    </p>{/if}
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
  fieldset {
    border: 0;
    padding: 0;
    margin: 1rem 0;
    display: grid;
    gap: 0.75rem;
  }
  label {
    display: flex;
    gap: 0.5rem;
    align-items: start;
  }
  .actions {
    display: flex;
    gap: 0.75rem;
    flex-wrap: wrap;
  }
  .muted {
    color: var(--foreground-muted);
  }
</style>
