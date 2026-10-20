<script lang="ts">
  import type { Snippet } from 'svelte';
  import type { ResourceSubmission } from './resourceSubmission.svelte';

  let {
    title,
    creating,
    allowed,
    submission,
    referenceError = null,
    enrolling = false,
    children,
    enrollment
  }: {
    title: string;
    creating: boolean;
    allowed: boolean;
    submission: ResourceSubmission;
    referenceError?: unknown;
    enrolling?: boolean;
    children: Snippet;
    enrollment?: Snippet;
  } = $props();
</script>

{#snippet actions()}
  <div class="actions">
    <button
      class="button button-primary"
      type="submit"
      disabled={submission.busy || enrolling || !allowed}
      >{submission.busy
        ? 'Saving…'
        : submission.submitted
          ? 'Retry save'
          : 'Save'}</button
    ><button
      class="button button-secondary"
      type="button"
      disabled={submission.busy || enrolling}
      onclick={submission.cancel}>Cancel</button
    >
    {#if submission.submitted}<button
        class="button button-secondary"
        type="button"
        disabled={submission.busy || !allowed}
        onclick={submission.editRequest}>Edit request</button
      >
      <p>
        Retry keeps the original submission to recover a lost response.
      </p>{/if}
  </div>
{/snippet}

<section class="card editor" aria-label="Code-mode editor">
  <h2>{creating ? 'Create' : 'Edit'} {title}</h2>
  {#if submission.error}<p role="alert" class="field-error">
      {submission.error}
    </p>{/if}
  {#if referenceError}<p role="alert" class="field-error">
      Some choices could not be loaded. Existing selections are retained. Cancel
      and reopen to retry.
    </p>{/if}
  <form
    class="form-field"
    onsubmit={submission.save}
    oninput={submission.markDirty}
    onchange={submission.markDirty}
  >
    <fieldset disabled={submission.busy || submission.submitted || !allowed}>
      {@render children()}
    </fieldset>
    {@render actions()}
  </form>
  {@render enrollment?.()}
</section>

<style>
  .editor,
  form,
  form :global(fieldset) {
    display: grid;
    gap: 0.75rem;
  }
  .editor {
    padding: 1.25rem;
  }
  form :global(fieldset) {
    min-width: 0;
    padding: 0;
    margin: 0;
  }
  h2 {
    font-size: 1.25rem;
    font-weight: 600;
  }
  form :global(label),
  form :global(legend) {
    font-size: 0.875rem;
    font-weight: 600;
  }
  p,
  form :global(p) {
    color: var(--foreground-subtle);
    font-size: 0.875rem;
    line-height: 1.6;
  }
  form :global(.choice),
  .actions {
    display: flex;
    align-items: center;
    gap: 0.65rem;
  }
  form :global(.choice input) {
    width: auto;
  }
  .field-error {
    color: var(--danger);
  }
</style>
