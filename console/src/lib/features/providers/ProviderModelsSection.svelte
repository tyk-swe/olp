<script lang="ts">
  import { providerKeys } from '$lib/features/providers/providerKeys';

  import { resolve } from '$app/paths';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import {
    errorMessage as providerDetailError,
    isEtagMismatch
  } from '$lib/api/http';
  import CursorPagination from '$lib/components/CursorPagination.svelte';
  import { formatDate } from '$lib/format';
  import {
    certifyProviderModel,
    declareProviderModels,
    discoverProviderModels,
    getProviderCapabilityOptions,
    setProviderModel,
    type CapabilityCertification,
    type CapabilityDeclaration,
    type ProviderModel
  } from '$lib/features/providers/models';
  import { probeProvider, type Provider } from '$lib/features/providers/api';
  import {
    cursorPaginationProps,
    resetCursor,
    type CursorHistory
  } from '$lib/lists/pagination';
  import CapabilityReview from '$lib/features/providers/CapabilityReview.svelte';
  import {
    activationReady,
    certificationPrerequisiteReady,
    DISABLED_EDIT_NOTE,
    parseManualModelNames,
    providerDisabled
  } from '$lib/features/providers/providerEditor';
  import type { RunProviderAction } from './providerEditor';
  import type { ProviderModelPage } from './models';

  let {
    current,
    modelSnapshot,
    loading,
    error,
    reloadModels,
    busy,
    canManage,
    run,
    reloadVersion,
    pageState = $bindable(),
    onProviderChanged,
    onError,
    onConflict,
    onNotice
  }: {
    current: Provider;
    modelSnapshot: ProviderModelPage | undefined;
    loading: boolean;
    error: unknown;
    reloadModels: () => Promise<void>;
    busy: string;
    canManage: boolean;
    run: RunProviderAction;
    reloadVersion: number;
    pageState: CursorHistory;
    onProviderChanged: () => Promise<void>;
    onError: (message: string) => void;
    onConflict: () => void;
    onNotice: (message: string) => void;
  } = $props();

  const queryClient = useQueryClient();
  let manualModelNames = $state('');
  let certificationResults = $state<Record<string, CapabilityCertification>>(
    {}
  );
  let previousProviderEtag = $state('');
  // Discovery, manual declaration and capability review all fail with the same
  // 409 on a disabled provider, so they stay locked until it is a draft again.
  const editingLocked = $derived(providerDisabled(current));

  const capabilityOptions = createQuery(() => ({
    queryKey: providerKeys.capabilityOptions(current.configuration.kind),
    queryFn: ({ signal }) =>
      getProviderCapabilityOptions(current.configuration.kind, signal)
  }));
  const visibleModelPage = $derived(modelSnapshot);
  $effect(() => {
    if (isEtagMismatch(error)) onConflict();
  });

  $effect(() => {
    if (current.etag === previousProviderEtag) return;
    previousProviderEtag = current.etag;
    certificationResults = {};
  });

  async function refresh(resetToFirstPage: boolean) {
    if (resetToFirstPage) resetCursor(pageState);
    await onProviderChanged();
  }

  async function discover() {
    if (!canManage || editingLocked) return;
    await run('detail-discover', async () => {
      const updated = await discoverProviderModels(current);
      certificationResults = {};
      await refresh(true);
      await queryClient.invalidateQueries({
        queryKey: providerKeys.summaries
      });
      onNotice(
        `${updated.model_count} model${updated.model_count === 1 ? '' : 's'} discovered. Review capabilities before activation.`
      );
    });
  }

  async function declareModels() {
    if (!canManage || editingLocked) return;
    const names = parseManualModelNames(manualModelNames);
    if (!names.length) {
      onError('Enter at least one upstream model identifier.');
      return;
    }
    await run('detail-declare', async () => {
      const updated = await declareProviderModels(current, names);
      manualModelNames = '';
      certificationResults = {};
      await refresh(true);
      await queryClient.invalidateQueries({
        queryKey: providerKeys.summaries
      });
      onNotice(
        `${updated.model_count} manually declared model${updated.model_count === 1 ? '' : 's'} ready for capability review.`
      );
    });
  }

  async function reviewModel(
    provider: Provider,
    model: ProviderModel,
    enabled: boolean,
    capabilities: CapabilityDeclaration[]
  ) {
    return run(`model-${model.id}`, async () => {
      await setProviderModel(provider, model.id, enabled, capabilities);
      certificationResults = {};
      await refresh(false);
      await queryClient.invalidateQueries({
        queryKey: providerKeys.summaries
      });
      onNotice('Capability review saved with declared provenance.');
    });
  }

  async function certifyModel(provider: Provider, modelId: string) {
    await run(`certify-${modelId}`, async () => {
      if (!certificationPrerequisiteReady(provider)) {
        const probe = await probeProvider(provider);
        if (!probe.succeeded) throw new Error(probe.detail);
        provider = {
          ...provider,
          last_probe_at: probe.checked_at,
          last_probe_status: 'succeeded',
          last_probe_detail: probe.detail
        };
      }
      const result = await certifyProviderModel(provider, modelId);
      certificationResults = { ...certificationResults, [modelId]: result };

      await refresh(false);
      await queryClient.invalidateQueries({
        queryKey: providerKeys.summaries
      });
      onNotice(
        `${result.certified_count} of ${result.attempted_count} reviewed tuples passed server certification. Test the completed draft before activation.`
      );
    });
  }
</script>

<section class="card editor models" aria-labelledby="models-heading">
  <div class="section-heading">
    <div>
      <p class="eyebrow">Discovery</p>
      <h2 id="models-heading">Models and capabilities</h2>
    </div>
    <a class="button button-secondary" href={resolve('/models')}
      >Inventory view</a
    >
  </div>
  <div class="discovery-row">
    <p class="muted">
      Refresh the inventory from the upstream model-list API. Existing
      capability certification is reconciled server-side.
    </p>
    <button
      class="button button-secondary"
      type="button"
      onclick={discover}
      disabled={!canManage || Boolean(busy) || editingLocked}
      >{busy === 'detail-discover'
        ? 'Discovering…'
        : 'Run upstream discovery'}</button
    >
  </div>
  {#if editingLocked}<p class="locked-note">{DISABLED_EDIT_NOTE}</p>{/if}
  {#if canManage && !editingLocked && current.configuration.kind === 'openai_compatible'}<details
      class="manual-fallback"
    >
      <summary>Manual model identifiers</summary>
      <p>
        Use only if this compatible endpoint has no list API. Models remain
        disabled until capability review.
      </p>
      <div class="form-field">
        <label for="manual-models-detail">Upstream model identifiers</label
        ><textarea
          id="manual-models-detail"
          bind:value={manualModelNames}
          placeholder="model-a&#10;model-b"></textarea>
      </div>
      <button
        class="button button-secondary"
        type="button"
        onclick={declareModels}
        disabled={!canManage || Boolean(busy)}
        >{busy === 'detail-declare'
          ? 'Adding…'
          : 'Add identifiers for review'}</button
      >
    </details>{/if}
  {#if Boolean(error)}<div class="inline-problem" role="alert">
      {providerDetailError(error)} The last loaded model page remains available below.
      <button
        class="button button-secondary"
        type="button"
        onclick={() => reloadModels()}>Retry</button
      >
    </div>{/if}
  {#if current.model_count === 0}<div class="empty-state">
      <p>No models have been discovered.</p>
    </div>
  {:else if loading && !visibleModelPage}<div
      class="loading-state"
      role="status"
    >
      Loading models…
    </div>
  {:else if visibleModelPage}
    {@const modelPage = visibleModelPage}
    <div class="table-shell">
      <table class="data-table">
        <thead><tr><th>Model</th><th>Explicit capability review</th></tr></thead
        ><tbody>
          {#each modelPage.items as model (model.id)}<tr
              ><td
                ><strong>{model.display_name}</strong><br /><code
                  >{model.upstream_model}</code
                >{#if model.discovered_at}<br /><small class="discovered"
                    >Discovered {formatDate(model.discovered_at)}</small
                  >{/if}</td
              ><td>
                <CapabilityReview
                  {model}
                  providerEtag={modelPage.provider.etag}
                  options={capabilityOptions.data?.capabilities ?? []}
                  optionsPending={capabilityOptions.isPending}
                  optionsError={capabilityOptions.isError}
                  disabled={!canManage || Boolean(busy) || editingLocked}
                  {reloadVersion}
                  certification={certificationResults[model.id]}
                  onSave={(enabled, capabilities, providerEtag) =>
                    reviewModel(
                      { ...modelPage.provider, etag: providerEtag },
                      model,
                      enabled,
                      capabilities
                    )}
                />
                <div class="certification-action">
                  <button
                    class="button button-secondary"
                    type="button"
                    onclick={() => certifyModel(modelPage.provider, model.id)}
                    disabled={!canManage ||
                      Boolean(busy) ||
                      editingLocked ||
                      !model.capabilities.length}
                    >{busy === `certify-${model.id}`
                      ? 'Server-certifying…'
                      : 'Server-certify capabilities'}</button
                  >
                  {#if certificationResults[model.id]}{@const result =
                      certificationResults[model.id]}<span
                      class:success={result.status === 'succeeded'}
                      class:warning={result.status !== 'succeeded'}
                      >{result.certified_count}/{result.attempted_count} certified</span
                    >{/if}
                </div>
              </td></tr
            >{/each}
        </tbody>
      </table>
    </div>
    <CursorPagination
      {...cursorPaginationProps(pageState, modelPage.nextCursor)}
      label="Provider model pages"
    />
  {/if}
  {#if !editingLocked && !activationReady(current)}<p class="audit-note">
      Every native and compatible tuple requires fresh server-owned
      certification. After the last change or certification, run the
      completed-draft connection test before activation.
    </p>{/if}
</section>

<style>
  .editor {
    margin-top: 1.25rem;
    padding: clamp(1.15rem, 3vw, 1.75rem);
  }
  h2 {
    margin: 0 0 0.85rem;
    font-size: 1.15rem;
    font-weight: 750;
    letter-spacing: -0.025em;
  }
  .muted,
  .audit-note,
  .locked-note {
    color: var(--foreground-muted);
  }
  .locked-note {
    margin: 1rem 0 0;
    font-size: 0.8rem;
  }
  .section-heading {
    display: flex;
    align-items: flex-start;
    justify-content: space-between;
    gap: 1rem;
  }
  .discovery-row {
    display: flex;
    align-items: end;
    gap: 0.6rem;
  }
  .certification-action {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 0.6rem;
    margin-top: 0.75rem;
    padding-top: 0.75rem;
    border-top: 1px solid var(--border);
    color: var(--foreground-muted);
    font-size: 0.75rem;
  }
  .discovered {
    color: var(--foreground-muted);
    font-size: 0.7rem;
  }
  .models {
    max-width: none;
  }
  .manual-fallback {
    margin-top: 1rem;
    padding: 0.75rem;
    border: 1px solid var(--border);
    border-radius: 0.375rem;
  }
  .manual-fallback summary {
    min-height: 2.75rem;
    font-weight: 720;
  }
  .manual-fallback p {
    color: var(--foreground-muted);
    font-size: 0.78rem;
  }
  .manual-fallback textarea {
    min-height: 5rem;
  }
  code {
    font:
      0.75rem 'JetBrains Mono Variable',
      monospace;
  }
  @media (max-width: 42rem) {
    .discovery-row {
      display: grid;
    }
  }
</style>
