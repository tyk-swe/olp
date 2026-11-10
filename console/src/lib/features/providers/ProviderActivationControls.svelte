<script lang="ts">
  import { confirmAction } from '#lib/components/confirm.js';
  import type { Provider } from './providers';
  import type { ProviderEditor } from './providerEditor.svelte';
  import {
    activationReady,
    capabilitiesCertified,
    probeReady,
    providerDisabled
  } from './providerEditor';
  import { formatDate, stateLabel } from '#lib/format.js';
  let {
    current,
    editor,
    canSave
  }: { current: Provider; editor: ProviderEditor; canSave: boolean } = $props();
  const editorView = $derived(editor.view);
  const busy = $derived(editorView.busy);
  const canManage = $derived(editor.canManage);
  const dirty = $derived(editorView.dirty);
  const pendingRefresh = $derived(editorView.refreshPending);
  const editingLocked = $derived(providerDisabled(current));
  const hasDraft = $derived(
    current.state === 'draft' ||
      (current.state === 'active' && current.pending_activation)
  );
  async function disable() {
    if (
      !(await confirmAction({
        title: `Disable ${current.name}?`,
        body: `Revision ${current.active_revision} stops serving as soon as the next runtime generation is published.`,
        confirmLabel: 'Disable',
        danger: true
      }))
    )
      return;
    await editor.disable();
  }
</script>

{#if hasDraft}
  <ol
    class="activation-checklist compact"
    aria-label="Provider activation requirements"
  >
    <li class:complete={capabilitiesCertified(current)}>
      {capabilitiesCertified(current) ? '✓' : '1'} Capabilities certified
    </li>
    <li class:complete={probeReady(current)}>
      {probeReady(current) ? '✓' : '2'} Completed draft tested
    </li>
  </ol>
  {#if !current.connector_ready}<p class="live-note">
      This build carries no connector for {stateLabel(
        current.configuration.kind
      )}
      providers, so the draft cannot be activated.
    </p>{/if}
{:else if editingLocked}
  <p class="live-note">
    This provider is disabled. No revision is serving traffic. Restore it as a
    draft to edit, re-certify, and activate it again.
  </p>
{:else if current.active_revision != null}
  <p class="live-note">
    Revision {current.active_revision} is live. No changes are pending.
  </p>
{/if}
{#if dirty}<p class="live-note">
    Save the complete configuration draft before testing or activating it.
  </p>{/if}
<div class="form-actions">
  <button
    class="button button-secondary"
    type="button"
    onclick={editor.saveDraft}
    disabled={!canManage ||
      Boolean(busy) ||
      Boolean(pendingRefresh) ||
      !canSave ||
      editingLocked}>Save draft</button
  >
  {#if canManage && hasDraft}
    <button
      class="button button-secondary"
      type="button"
      onclick={editor.probe}
      disabled={!canManage ||
        Boolean(busy) ||
        Boolean(pendingRefresh) ||
        dirty ||
        !canSave ||
        !capabilitiesCertified(current)}
      >{busy === 'detail-probe'
        ? 'Testing completed draft…'
        : 'Test completed draft'}</button
    >
    <button
      class="button button-primary"
      type="button"
      onclick={editor.activate}
      disabled={!canManage ||
        Boolean(busy) ||
        Boolean(pendingRefresh) ||
        dirty ||
        !canSave ||
        !activationReady(current)}>Activate changes</button
    >
  {/if}
  {#if canManage && editingLocked}
    <button
      class="button button-primary"
      type="button"
      onclick={editor.restoreDraft}
      disabled={Boolean(busy) || Boolean(pendingRefresh)}
      >{busy === 'detail-restore-draft'
        ? 'Restoring provider…'
        : 'Restore provider as draft'}</button
    >
  {:else if canManage && current.active_revision != null}
    <button
      class="button button-secondary button-danger"
      type="button"
      onclick={disable}
      disabled={Boolean(busy) || Boolean(pendingRefresh)}
      >{busy === 'detail-disable' ? 'Disabling…' : 'Disable provider'}</button
    >
  {/if}
</div>
{#if pendingRefresh}<p class="inline-problem" role="status">
    The provider change succeeded. Retry refreshing the view before another
    change.
    <button
      class="button button-secondary"
      type="button"
      disabled={Boolean(busy)}
      onclick={editor.retryRefresh}>Retry refresh</button
    >
  </p>{/if}
{#if editorView.referenceConflict}<p class="inline-problem" role="alert">
    {editorView.referenceConflict} Retarget every route that still uses this provider's
    models, and let live media jobs finish, before disabling it.
  </p>{/if}
{#if current.last_probe_at}<p class="audit-note">
    Last probe {formatDate(current.last_probe_at)}: {current.last_probe_status}
    — {current.last_probe_detail}
  </p>{/if}

<style>
  .audit-note,
  .live-note {
    color: var(--foreground-muted);
  }
  .live-note {
    margin: 1rem 0 0;
    font-size: var(--text-body-sm);
  }
  .form-actions {
    display: flex;
    flex-wrap: wrap;
    gap: 0.65rem;
    margin-top: 1.35rem;
  }
  .activation-checklist {
    display: grid;
    gap: 0.4rem;
    margin: 1rem 0 0;
    padding: 0;
    list-style: none;
    color: var(--foreground-muted);
    font-size: var(--text-body-sm);
  }
  .activation-checklist li {
    min-height: 1.5rem;
  }
  .activation-checklist li.complete {
    color: var(--success);
    font-weight: 500;
  }
  .activation-checklist.compact {
    margin-top: 1.1rem;
  }
</style>
