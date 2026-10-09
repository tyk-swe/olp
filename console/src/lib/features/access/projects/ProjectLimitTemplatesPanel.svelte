<script lang="ts">
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage } from '$lib/api/http';
  import PolicyFields from '../limit-templates/TemplateFields.svelte';
  import {
    templateError,
    templateForm,
    templateInput
  } from '../limit-templates/policy';
  import {
    getProjectLimitTemplates,
    putProjectLimitTemplates,
    type ProjectLimitTemplates
  } from './api';
  import { projectKeys } from './projectKeys';

  let {
    projectId,
    editable = true
  }: { projectId: string; editable?: boolean } = $props();
  const client = useQueryClient();
  const policy = createQuery(() => ({
    queryKey: [...projectKeys.root, 'limit-templates', projectId],
    queryFn: ({ signal }) => getProjectLimitTemplates(projectId, signal)
  }));
  let form = $state(templateForm());
  let etag = $state('');
  let loaded = $state('');
  let busy = $state(false);
  let error = $state('');
  let notice = $state('');

  function load(value: ProjectLimitTemplates) {
    form = templateForm(value.templates);
    etag = value.etag;
    loaded = JSON.stringify(templateInput(form));
  }
  $effect(() => {
    if (
      policy.data &&
      (!etag ||
        (policy.data.etag !== etag &&
          JSON.stringify(templateInput(form)) === loaded))
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
    error = templateError(form);
    notice = '';
    if (error) return;
    busy = true;
    try {
      load(
        await putProjectLimitTemplates(projectId, etag, templateInput(form))
      );
      await client.invalidateQueries({ queryKey: projectKeys.root });
      notice = 'Limit templates saved.';
    } catch (cause) {
      error = errorMessage(cause);
    } finally {
      busy = false;
    }
  }
</script>

<section aria-label="Project limit templates">
  {#if policy.isPending}
    <p role="status">Loading limit templates…</p>
  {:else if policy.isError}
    <p class="inline-problem" role="alert">{errorMessage(policy.error)}</p>
    <button class="button button-secondary" type="button" onclick={reload}
      >Retry limit templates</button
    >
  {:else if etag}
    <form onsubmit={submit}>
      <p class="muted">
        Manage shared limit ceilings for this project. Changes apply to every
        referencing key, end-user policy and budget group.
      </p>
      <PolicyFields bind:form disabled={busy || !editable} />
      {#if error}<p class="inline-problem" role="alert">{error}</p>{/if}
      {#if notice}<p class="notice" role="status">{notice}</p>{/if}
      <div class="actions">
        {#if editable}<button
            class="button button-primary"
            type="submit"
            disabled={busy}>{busy ? 'Saving…' : 'Save limit templates'}</button
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
