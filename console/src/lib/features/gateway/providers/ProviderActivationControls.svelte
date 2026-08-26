<script lang="ts">
  import { useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage as providerActionError } from '$lib/api/http';
  import {
    activateProvider,
    disableProvider,
    getProvider,
    isProviderInUse,
    probeProvider,
    restoreProviderAsDraft,
    type Provider
  } from '$lib/api/management/providers';
  import {
    activationReady,
    capabilitiesCertified,
    probeReady,
    probeSummary
  } from './providerEditor';
  import {
    invalidateProviderModelConsumers,
    invalidateProviderSummaries
  } from './providerCache';
  import type { RunProviderAction } from './providerDetailCoordination';
  import { formatDate } from '$lib/format';

  let {
    current,
    busy,
    canManage,
    canSave,
    run,
    onSave,
    onAcceptProvider,
    onRefetchProvider,
    onNotice
  }: {
    current: Provider;
    busy: string;
    canManage: boolean;
    canSave: boolean;
    run: RunProviderAction;
    onSave: () => void;
    onAcceptProvider: (provider: Provider) => void;
    onRefetchProvider: () => Promise<boolean>;
    onNotice: (message: string) => void;
  } = $props();

  const queryClient = useQueryClient();
  let referenceConflict = $state('');

  function invalidateProviderViews() {
    return Promise.all([
      invalidateProviderSummaries(queryClient),
      invalidateProviderModelConsumers(queryClient),
      queryClient.invalidateQueries({
        queryKey: ['provider-credentials', current.id]
      }),
      queryClient.invalidateQueries({
        queryKey: ['provider-revisions', current.id]
      })
    ]);
  }

  async function testDraft() {
    await run('detail-probe', async () => {
      const probe = await probeProvider(current);
      if (!probe.succeeded) throw new Error(probe.detail);
      onAcceptProvider(await getProvider(current.id));
      await invalidateProviderSummaries(queryClient);
      onNotice(`Connection succeeded: ${probeSummary(probe)}`);
    });
  }

  async function activate() {
    await run('detail-activate', async () => {
      const generation = await activateProvider(current);
      const refreshed = await onRefetchProvider();
      await invalidateProviderViews();
      if (refreshed) {
        onNotice(`Activated in runtime generation ${generation}.`);
      }
    });
  }

  async function disable() {
    if (!canManage) return;
    if (
      !confirm(
        `Disable “${current.name}”? Revision ${current.active_revision} stops serving as soon as the next runtime generation is published.`
      )
    )
      return;
    referenceConflict = '';
    await run('detail-disable', async () => {
      let generation: number | null;
      try {
        generation = await disableProvider(current);
      } catch (error) {
        // The server refuses while a route still targets one of this
        // provider's models, or an upstream media job is still live.
        if (!isProviderInUse(error)) throw error;
        referenceConflict = providerActionError(error);
        return;
      }
      const refreshed = await onRefetchProvider();
      await invalidateProviderViews();
      if (refreshed) {
        onNotice(
          generation == null
            ? 'Provider disabled. No revision is serving traffic.'
            : `Provider disabled in runtime generation ${generation}.`
        );
      }
    });
  }

  async function restoreDraft() {
    if (!canManage) return;
    referenceConflict = '';
    await run('detail-restore-draft', async () => {
      const restored = await restoreProviderAsDraft(current);
      onAcceptProvider(restored);
      await invalidateProviderViews();
      onNotice(
        'Provider restored as a draft. Stored capabilities are declared again: re-certify and test before activation.'
      );
    });
  }
</script>

{#if current.state === 'draft'}
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
      This build carries no connector for {current.kind.replaceAll('_', ' ')}
      providers, so the draft cannot be activated.
    </p>{/if}
{:else if current.state === 'disabled'}
  <p class="live-note">
    This provider is disabled. No revision is serving traffic. Restore it as a
    draft to edit, re-certify, and activate it again.
  </p>
{:else if current.active_revision != null}
  <p class="live-note">
    Revision {current.active_revision} is live. No changes are pending.
  </p>
{/if}
<div class="form-actions">
  <button
    class="button button-secondary"
    type="button"
    onclick={onSave}
    disabled={!canManage || Boolean(busy) || !canSave}>Save draft</button
  >
  {#if canManage && current.state === 'draft'}
    <button
      class="button button-secondary"
      type="button"
      onclick={testDraft}
      disabled={!canManage || Boolean(busy) || !capabilitiesCertified(current)}
      >{busy === 'detail-probe'
        ? 'Testing completed draft…'
        : 'Test completed draft'}</button
    >
    <button
      class="button button-primary"
      type="button"
      onclick={activate}
      disabled={!canManage || Boolean(busy) || !activationReady(current)}
      >Activate changes</button
    >
  {/if}
  {#if canManage && current.state === 'disabled'}
    <button
      class="button button-primary"
      type="button"
      onclick={restoreDraft}
      disabled={Boolean(busy)}
      >{busy === 'detail-restore-draft'
        ? 'Restoring provider…'
        : 'Restore provider as draft'}</button
    >
  {:else if canManage && current.active_revision != null}
    <button
      class="button button-secondary danger-button"
      type="button"
      onclick={disable}
      disabled={Boolean(busy)}
      >{busy === 'detail-disable' ? 'Disabling…' : 'Disable provider'}</button
    >
  {/if}
</div>
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
    font-size: 0.8rem;
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
    font-size: 0.8rem;
  }
  .activation-checklist li {
    min-height: 1.5rem;
  }
  .activation-checklist li.complete {
    color: var(--success);
    font-weight: 700;
  }
  .activation-checklist.compact {
    margin-top: 1.1rem;
  }
</style>
