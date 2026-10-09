<script lang="ts">
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage } from '$lib/api/http';
  import PolicyFields from '../attribution/AttributionFields.svelte';
  import {
    attributionError,
    attributionForm,
    attributionInput
  } from '../attribution/policy';
  import {
    getProjectAttributionPolicy,
    putProjectAttributionPolicy,
    type ProjectAttributionPolicy
  } from './api';
  import { projectKeys } from './projectKeys';

  let {
    projectId,
    editable = true
  }: { projectId: string; editable?: boolean } = $props();
  const client = useQueryClient();
  const policy = createQuery(() => ({
    queryKey: [...projectKeys.root, 'attribution-policy', projectId],
    queryFn: ({ signal }) => getProjectAttributionPolicy(projectId, signal)
  }));
  let form = $state(attributionForm());
  let etag = $state('');
  let loaded = $state('');
  let busy = $state(false);
  let error = $state('');
  let notice = $state('');

  function load(value: ProjectAttributionPolicy) {
    form = attributionForm(value.policy);
    etag = value.etag;
    loaded = JSON.stringify(attributionInput(form));
  }
  $effect(() => {
    if (
      policy.data &&
      (!etag ||
        (policy.data.etag !== etag &&
          JSON.stringify(attributionInput(form)) === loaded))
    )
      load(policy.data);
  });

  async function reload() {
    const result = await policy.refetch();
    if (result.data) {
      load(result.data);
      error = notice = '';
    }
  }

  async function submit(event: SubmitEvent) {
    event.preventDefault();
    if (busy || !editable) return;
    error = attributionError(form);
    notice = '';
    if (error) return;
    busy = true;
    try {
      load(
        await putProjectAttributionPolicy(
          projectId,
          etag,
          attributionInput(form)
        )
      );
      await client.invalidateQueries({ queryKey: projectKeys.root });
      notice = 'Attribution policy saved.';
    } catch (cause) {
      error = errorMessage(cause);
    } finally {
      busy = false;
    }
  }
</script>

<section aria-label="Project attribution policy">
  {#if policy.isPending}
    <p role="status">Loading attribution policy…</p>
  {:else if policy.isError}
    <p class="inline-problem" role="alert">{errorMessage(policy.error)}</p>
    <button class="button button-secondary" type="button" onclick={reload}
      >Retry attribution policy</button
    >
  {:else if etag}
    <form onsubmit={submit}>
      <p class="muted">
        Labels apply to every inference key in this project. Pinned values
        cannot be overridden by keys or callers. Clear all fields to remove the
        project policy.
      </p>
      <PolicyFields
        bind:form
        prefix={`project-${projectId}-attribution`}
        disabled={busy || !editable}
      />
      {#if error}<p class="inline-problem" role="alert">{error}</p>{/if}
      {#if notice}<p class="notice" role="status">{notice}</p>{/if}
      <div class="actions">
        {#if editable}<button
            class="button button-primary"
            type="submit"
            disabled={busy}
            >{busy ? 'Saving…' : 'Save attribution policy'}</button
          >{/if}
        <button
          type="button"
          class="button button-secondary"
          disabled={busy}
          onclick={reload}>Reload saved policy</button
        >
      </div>
    </form>
  {/if}
</section>

<style>
  section {
    margin: 1.5rem 0;
    padding: 1rem 0;
    border-top: 1px solid var(--border-hairline);
    border-bottom: 1px solid var(--border-hairline);
  }
  .actions {
    display: flex;
    flex-wrap: wrap;
    gap: 0.75rem;
  }
</style>
