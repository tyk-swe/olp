<script lang="ts">
  import {
    useMutationIntent,
    usePrincipalLifetime
  } from '$lib/forms/mutationIntent.svelte';

  import { overviewKeys } from '$lib/features/overview/overviewKeys';
  import { providerKeys } from '$lib/features/providers/providerKeys';

  import { useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage as providerActionError } from '$lib/api/http';
  import {
    activateProvider,
    disableProvider,
    isProviderInUse,
    probeProvider,
    restoreProviderAsDraft,
    type Provider
  } from '$lib/features/providers/api/providers';
  import {
    activationReady,
    capabilitiesCertified,
    disableNotice,
    probeReady,
    probeSummary,
    providerDisabled
  } from '$lib/features/providers/providerEditor';
  import type { RunProviderAction } from './providerEditor';
  import { formatDate, stateLabel } from '$lib/format';

  const activateProviderIntent = useMutationIntent();
  const disableProviderIntent = useMutationIntent();
  const restoreProviderAsDraftIntent = useMutationIntent();

  let {
    current,
    busy,
    canManage,
    canSave,
    dirty = false,
    run,
    onSave,
    onProviderChanged,
    onRefetchProvider,
    onNotice
  }: {
    current: Provider;
    busy: string;
    canManage: boolean;
    canSave: boolean;
    dirty?: boolean;
    run: RunProviderAction;
    onSave: () => void;
    onProviderChanged: () => Promise<void>;
    onRefetchProvider: () => Promise<boolean>;
    onNotice: (message: string) => void;
  } = $props();

  const queryClient = useQueryClient();
  let referenceConflict = $state('');
  let pendingRefresh = $state<{
    providerId: string;
    refresh: 'provider' | 'changed';
    notice: string;
  } | null>(null);
  usePrincipalLifetime(() => {
    pendingRefresh = null;
    referenceConflict = '';
  });
  $effect(() => {
    if (pendingRefresh && pendingRefresh.providerId !== current.id)
      pendingRefresh = null;
  });
  const editingLocked = $derived(providerDisabled(current));
  const hasDraft = $derived(
    current.state === 'draft' ||
      (current.state === 'active' && current.pending_activation)
  );

  // Every action clears the reference conflict, so a later success never
  // renders beside the red banner the previous disable attempt left behind.
  const runAction: RunProviderAction = (label, action) => {
    referenceConflict = '';
    return run(label, action);
  };

  // Save draft runs through the parent, so it clears the banner on its own way
  // out rather than through `runAction`.
  function save() {
    if (pendingRefresh) return;
    referenceConflict = '';
    onSave();
  }

  function invalidateProviderViews() {
    return Promise.all([
      queryClient.invalidateQueries({
        queryKey: providerKeys.summaries
      }),
      queryClient.invalidateQueries({
        queryKey: providerKeys.modelCatalog
      }),
      queryClient.invalidateQueries({
        queryKey: providerKeys.credentials(current.id)
      }),
      queryClient.invalidateQueries({
        queryKey: providerKeys.revisionsOf(current.id)
      }),
      queryClient.invalidateQueries({ queryKey: overviewKeys.root })
    ]);
  }

  async function testDraft() {
    if (pendingRefresh) return;
    await runAction('detail-probe', async (isCurrent) => {
      const { result: probe } = await probeProvider(current);
      if (!isCurrent()) return;
      if (!probe.succeeded) throw new Error(probe.detail);
      await onProviderChanged();
      if (!isCurrent()) return;
      await queryClient.invalidateQueries({
        queryKey: providerKeys.summaries
      });
      if (!isCurrent()) return;
      onNotice(`Connection succeeded: ${probeSummary(probe)}`);
    });
  }

  async function activate() {
    if (pendingRefresh) return;
    await runAction('detail-activate', async (isCurrent) => {
      const submission = activateProviderIntent.prepare(current.id, () => ({
        id: current.id,
        etag: current.etag
      }));
      const generation = await activateProvider(
        submission.input,
        submission.requestId
      );
      if (!isCurrent()) return;
      activateProviderIntent.reset();
      pendingRefresh = {
        providerId: current.id,
        refresh: 'provider',
        notice: `Activated in runtime generation ${generation}.`
      };
      await refreshConfirmedAction(isCurrent);
    });
  }

  async function disable() {
    if (!canManage || pendingRefresh) return;
    if (
      !confirm(
        `Disable “${current.name}”? Revision ${current.active_revision} stops serving as soon as the next runtime generation is published.`
      )
    )
      return;
    await runAction('detail-disable', async (isCurrent) => {
      let generation: number | null;
      try {
        const submission = disableProviderIntent.prepare(current.id, () => ({
          id: current.id,
          etag: current.etag
        }));
        generation = await disableProvider(
          submission.input,
          submission.requestId
        );
        if (!isCurrent()) return;
        disableProviderIntent.reset();
      } catch (error) {
        if (!isCurrent()) return;
        // The server refuses while a route still targets one of this
        // provider's models, or an upstream media job is still live.
        if (!isProviderInUse(error)) throw error;
        referenceConflict = providerActionError(error);
        return;
      }
      if (!isCurrent()) return;
      pendingRefresh = {
        providerId: current.id,
        refresh: 'provider',
        notice: disableNotice(generation)
      };
      await refreshConfirmedAction(isCurrent);
    });
  }

  async function restoreDraft() {
    if (!canManage || pendingRefresh) return;
    await runAction('detail-restore-draft', async (isCurrent) => {
      const submission = restoreProviderAsDraftIntent.prepare(
        current.id,
        () => ({ id: current.id, etag: current.etag })
      );
      await restoreProviderAsDraft(submission.input, submission.requestId);
      if (!isCurrent()) return;
      restoreProviderAsDraftIntent.reset();
      pendingRefresh = {
        providerId: current.id,
        refresh: 'changed',
        notice:
          'Provider restored as a draft. Stored capabilities are declared again: re-certify and test before activation.'
      };
      await refreshConfirmedAction(isCurrent);
    });
  }

  async function refreshConfirmedAction(isCurrent: () => boolean) {
    const confirmed = pendingRefresh;
    if (!confirmed || !isCurrent()) return;
    onNotice(confirmed.notice);
    try {
      if (confirmed.refresh === 'provider') {
        if (!(await onRefetchProvider()))
          throw new Error('The provider detail could not be refreshed.');
      } else await onProviderChanged();
      if (!isCurrent()) return;
      await invalidateProviderViews();
      if (!isCurrent()) return;
      if (pendingRefresh === confirmed) pendingRefresh = null;
      onNotice(confirmed.notice);
    } catch (cause) {
      if (!isCurrent()) return;
      throw new Error(
        `The provider change succeeded, but refreshing failed. ${providerActionError(cause)}`,
        { cause }
      );
    }
  }

  async function retryRefresh() {
    await runAction('detail-refresh', refreshConfirmedAction);
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
    onclick={save}
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
      onclick={testDraft}
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
      onclick={activate}
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
      onclick={restoreDraft}
      disabled={Boolean(busy) || Boolean(pendingRefresh)}
      >{busy === 'detail-restore-draft'
        ? 'Restoring provider…'
        : 'Restore provider as draft'}</button
    >
  {:else if canManage && current.active_revision != null}
    <button
      class="button button-secondary danger-button"
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
      onclick={retryRefresh}>Retry refresh</button
    >
  </p>{/if}
{#if referenceConflict}<p class="inline-problem" role="alert">
    {referenceConflict} Retarget every route that still uses this provider's models,
    and let live media jobs finish, before disabling it.
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
  .danger-button {
    color: var(--danger);
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
