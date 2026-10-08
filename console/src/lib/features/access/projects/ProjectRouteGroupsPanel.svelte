<script lang="ts">
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage } from '$lib/api/http';
  import PolicyFields from '../route-groups/GroupFields.svelte';
  import { groupError, groupForm, groupInput } from '../route-groups/policy';
  import {
    getProjectRouteGroups,
    putProjectRouteGroups,
    type ProjectRouteGroups
  } from './api';
  import { projectKeys } from './projectKeys';

  let {
    projectId,
    editable = true
  }: { projectId: string; editable?: boolean } = $props();
  const client = useQueryClient();
  const policy = createQuery(() => ({
    queryKey: [...projectKeys.root, 'route-groups', projectId],
    queryFn: ({ signal }) => getProjectRouteGroups(projectId, signal)
  }));
  let form = $state(groupForm());
  let etag = $state('');
  let loaded = $state('');
  let busy = $state(false);
  let error = $state('');
  let notice = $state('');

  function load(value: ProjectRouteGroups) {
    form = groupForm(value.groups);
    etag = value.etag;
    loaded = JSON.stringify(groupInput(form));
  }
  $effect(() => {
    if (
      policy.data &&
      (!etag ||
        (policy.data.etag !== etag &&
          JSON.stringify(groupInput(form)) === loaded))
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
    error = groupError(form);
    notice = '';
    if (error) return;
    busy = true;
    try {
      load(await putProjectRouteGroups(projectId, etag, groupInput(form)));
      await client.invalidateQueries({ queryKey: projectKeys.root });
      notice = 'Route groups saved.';
    } catch (cause) {
      error = errorMessage(cause);
    } finally {
      busy = false;
    }
  }
</script>

<section aria-label="Project route groups">
  {#if policy.isPending}
    <p role="status">Loading route groups…</p>
  {:else if policy.isError}
    <p class="inline-problem" role="alert">{errorMessage(policy.error)}</p>
    <button class="button button-secondary" type="button" onclick={reload}
      >Retry route groups</button
    >
  {:else if etag}
    <form onsubmit={submit}>
      <p class="muted">
        Manage named route sets for keys in this project. Membership changes
        update every key that references a group.
      </p>
      <PolicyFields
        bind:form
        prefix={`project-${projectId}-groups`}
        disabled={busy || !editable}
      />
      {#if error}<p class="inline-problem" role="alert">{error}</p>{/if}
      {#if notice}<p class="notice" role="status">{notice}</p>{/if}
      <div class="actions">
        {#if editable}<button
            class="button button-primary"
            type="submit"
            disabled={busy}>{busy ? 'Saving…' : 'Save route groups'}</button
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
