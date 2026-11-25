<script lang="ts">
  import { useAction } from '#lib/editing/action.svelte.js';
  import { notify } from '#lib/components/toasts.svelte.js';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { useRole } from '#lib/session/useRole.svelte.js';
  import { errorMessage } from '#lib/management/http.js';
  import { getPublication, putPublication } from './api';
  import { projectKeys } from './projectKeys';
  let { projectId }: { projectId: string } = $props();
  const access = useRole();
  const client = useQueryClient();
  const publication = createQuery(() => ({
    queryKey: projectKeys.publication(projectId),
    queryFn: ({ signal }) => getPublication(projectId, signal)
  }));
  let enabled = $state(false);
  let pricesPublic = $state(false);
  let etag = $state('');
  const action = useAction();
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
      action.error = '';
    }
  }
  async function save(event: SubmitEvent) {
    event.preventDefault();
    await action.run(
      () =>
        putPublication(projectId, {
          enabled,
          prices_public: pricesPublic,
          etag
        }),
      async (result) => {
        client.setQueryData(projectKeys.publication(projectId), result);
        etag = result.etag;
        notify('Catalog publication saved.');
      }
    );
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
  {#if action.error}<p role="alert" class="inline-problem">
      {action.error}
    </p>{/if}

  {#if !access.allows('PUT /api/v1/projects/{project_id}/catalog')}<p
      class="muted"
    >
      An installation owner can change public catalog publication.
    </p>{/if}
  <form onsubmit={save}>
    <fieldset
      disabled={action.busy ||
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
        disabled={action.busy ||
          !etag ||
          !access.allows('PUT /api/v1/projects/{project_id}/catalog')}
        >Save publication</button
      ><button
        class="button button-secondary"
        type="button"
        disabled={action.busy || publication.isFetching}
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
</style>
