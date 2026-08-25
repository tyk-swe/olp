<script lang="ts">
  import { useQueryClient } from '@tanstack/svelte-query';
  import {
    activateProvider,
    getProvider,
    probeProvider,
    type Provider
  } from '$lib/api/management/providers';
  import {
    activationReady,
    capabilitiesCertified,
    probeReady
  } from './providerEditor';
  import {
    invalidateProviderModelConsumers,
    invalidateProviderSummaries
  } from './providerCache';
  import type { RunProviderAction } from './providerDetailCoordination';

  let {
    current,
    busy,
    canSave,
    run,
    onSave,
    onAcceptProvider,
    onRefetchProvider,
    onNotice
  }: {
    current: Provider;
    busy: string;
    canSave: boolean;
    run: RunProviderAction;
    onSave: () => void;
    onAcceptProvider: (provider: Provider) => void;
    onRefetchProvider: () => Promise<boolean>;
    onNotice: (message: string) => void;
  } = $props();

  const queryClient = useQueryClient();

  async function testDraft() {
    await run('detail-probe', async () => {
      const probe = await probeProvider(current);
      if (!probe.succeeded) throw new Error(probe.detail);
      onAcceptProvider(await getProvider(current.id));
      await invalidateProviderSummaries(queryClient);
      onNotice(`Connection succeeded: ${probe.detail}`);
    });
  }

  async function activate() {
    await run('detail-activate', async () => {
      const generation = await activateProvider(current);
      const refreshed = await onRefetchProvider();
      await Promise.all([
        invalidateProviderSummaries(queryClient),
        invalidateProviderModelConsumers(queryClient),
        queryClient.invalidateQueries({
          queryKey: ['provider-credentials', current.id]
        }),
        queryClient.invalidateQueries({
          queryKey: ['provider-revisions', current.id]
        })
      ]);
      if (refreshed) {
        onNotice(`Activated in runtime generation ${generation}.`);
      }
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
{:else if current.active_revision != null}
  <p class="live-note">
    Revision {current.active_revision} is live. No changes are pending.
  </p>
{:else}
  <p class="live-note">
    This provider is disabled. No revision is serving traffic.
  </p>
{/if}
<div class="form-actions">
  <button
    class="button button-secondary"
    type="button"
    onclick={onSave}
    disabled={Boolean(busy) || !canSave}>Save draft</button
  >
  {#if current.state === 'draft'}
    <button
      class="button button-secondary"
      type="button"
      onclick={testDraft}
      disabled={Boolean(busy) || !capabilitiesCertified(current)}
      >{busy === 'detail-probe'
        ? 'Testing completed draft…'
        : 'Test completed draft'}</button
    >
    <button
      class="button button-primary"
      type="button"
      onclick={activate}
      disabled={Boolean(busy) || !activationReady(current)}
      >Activate changes</button
    >
  {/if}
</div>
{#if current.last_probe_at}<p class="audit-note">
    Last probe {new Date(current.last_probe_at).toLocaleString()}: {current.last_probe_status}
    — {current.last_probe_detail}
  </p>{/if}

<style>
  .audit-note,
  .live-note {
    color: var(--foreground-muted);
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
