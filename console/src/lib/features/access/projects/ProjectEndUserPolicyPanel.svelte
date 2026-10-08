<script lang="ts">
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage } from '$lib/api/http';
  import PolicyFields from '../end-users/PolicyFields.svelte';
  import { policyError, policyForm, policyInput } from '../end-users/policy';
  import {
    getProjectEndUserPolicy,
    putProjectEndUserPolicy,
    type ProjectEndUserPolicy
  } from './api';
  import { projectKeys } from './projectKeys';

  let {
    projectId,
    editable = true
  }: { projectId: string; editable?: boolean } = $props();
  const client = useQueryClient();
  const policy = createQuery(() => ({
    queryKey: [...projectKeys.root, 'end-user-policy', projectId],
    queryFn: ({ signal }) => getProjectEndUserPolicy(projectId, signal)
  }));
  let form = $state(policyForm());
  let etag = $state('');
  let loaded = $state('');
  let busy = $state(false);
  let error = $state('');
  let notice = $state('');

  function load(value: ProjectEndUserPolicy) {
    form = policyForm(value.policy);
    etag = value.etag;
    loaded = JSON.stringify(policyInput(form));
  }
  $effect(() => {
    if (
      policy.data &&
      (!etag ||
        (policy.data.etag !== etag &&
          JSON.stringify(policyInput(form)) === loaded))
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
    error = policyError(form);
    notice = '';
    if (error) return;
    busy = true;
    try {
      load(await putProjectEndUserPolicy(projectId, etag, policyInput(form)));
      await client.invalidateQueries({ queryKey: projectKeys.root });
      notice = 'End-user policy saved.';
    } catch (cause) {
      error = errorMessage(cause);
    } finally {
      busy = false;
    }
  }
</script>

<section aria-label="Project end-user policy">
  {#if policy.isPending}
    <p role="status">Loading end-user policy…</p>
  {:else if policy.isError}
    <p class="inline-problem" role="alert">{errorMessage(policy.error)}</p>
    <button class="button button-secondary" type="button" onclick={reload}
      >Retry end-user policy</button
    >
  {:else if etag}
    <form onsubmit={submit}>
      <p class="muted">
        Project limits are shared across its keys for the same end-user digest.
        Every inference key must select an identifier source while this policy
        is enabled.
      </p>
      <PolicyFields
        bind:form
        prefix={`project-${projectId}-end-user`}
        disabled={busy || !editable}
      />
      {#if error}<p class="inline-problem" role="alert">{error}</p>{/if}
      {#if notice}<p class="notice" role="status">{notice}</p>{/if}
      <div class="actions">
        {#if editable}<button
            class="button button-primary"
            type="submit"
            disabled={busy}>{busy ? 'Saving…' : 'Save end-user policy'}</button
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
