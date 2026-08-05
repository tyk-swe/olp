<script lang="ts">
  import { resolve } from '$app/paths';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import CursorPagination from '$lib/components/CursorPagination.svelte';
  import {
    certifyProviderModel,
    declareProviderModels,
    discoverProviderModels,
    getProvider,
    getProviderCapabilityOptions,
    probeProvider,
    setProviderModel,
    type CapabilityCertification,
    type CapabilityDeclaration,
    type Provider,
    type ProviderModel
  } from '$lib/api/management/providers';
  import CapabilityReview from './CapabilityReview.svelte';
  import {
    activationReady,
    certificationPrerequisiteReady,
    parseManualModelNames
  } from './providerEditor';
  import {
    fetchCoordinatedModelPage,
    installProviderWithModels,
    providerDetailError,
    providerModelPageKey,
    type CoordinatedModelPage,
    type RunProviderAction
  } from './providerDetailCoordination';
  import { invalidateProviderSummaries } from './providerCache';

  let {
    current,
    busy,
    run,
    reloadVersion,
    onAcceptProvider,
    onError,
    onNotice
  }: {
    current: Provider;
    busy: string;
    run: RunProviderAction;
    reloadVersion: number;
    onAcceptProvider: (provider: Provider) => void;
    onError: (message: string) => void;
    onNotice: (message: string) => void;
  } = $props();

  const queryClient = useQueryClient();
  let cursor = $state<string | undefined>();
  let history = $state<Array<string | undefined>>([]);
  let retainedModelPage = $state<CoordinatedModelPage>();
  let manualModelNames = $state('');
  let certificationResults = $state<Record<string, CapabilityCertification>>(
    {}
  );
  let previousProviderEtag = $state('');

  const capabilityOptions = createQuery(() => ({
    queryKey: ['provider-capability-options', current.kind],
    queryFn: ({ signal }) => getProviderCapabilityOptions(current.kind, signal)
  }));
  const models = createQuery(() => ({
    queryKey: providerModelPageKey(current.id, current, cursor),
    queryFn: ({ signal }) => fetchCoordinatedModelPage(current, cursor, signal),
    placeholderData: (previous: CoordinatedModelPage | undefined) => previous
  }));
  const visibleModelPage = $derived(models.data ?? retainedModelPage);

  $effect(() => {
    if (models.data) retainedModelPage = models.data;
  });

  $effect(() => {
    if (current.etag === previousProviderEtag) return;
    previousProviderEtag = current.etag;
    certificationResults = {};
  });

  async function install(updated: Provider, resetToFirstPage: boolean) {
    const targetCursor = resetToFirstPage ? undefined : cursor;
    await installProviderWithModels(
      queryClient,
      updated,
      targetCursor,
      (provider) => {
        previousProviderEtag = provider.etag;
        onAcceptProvider(provider);
      }
    );
    if (resetToFirstPage) {
      cursor = undefined;
      history = [];
    }
  }

  async function discover() {
    await run('detail-discover', async () => {
      const updated = await discoverProviderModels(current);
      certificationResults = {};
      await install(updated, true);
      await invalidateProviderSummaries(queryClient);
      onNotice(
        `${updated.model_count} model${updated.model_count === 1 ? '' : 's'} reviewed.`
      );
    });
  }

  async function declareModels() {
    const names = parseManualModelNames(manualModelNames);
    if (!names.length) {
      onError('Enter at least one upstream model identifier.');
      return;
    }
    await run('detail-declare', async () => {
      const updated = await declareProviderModels(current, names);
      manualModelNames = '';
      certificationResults = {};
      await install(updated, true);
      await invalidateProviderSummaries(queryClient);
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
      const updated = await setProviderModel(
        provider,
        model.id,
        enabled,
        capabilities
      );
      certificationResults = {};
      await install(updated, false);
      await invalidateProviderSummaries(queryClient);
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
      const updated = await getProvider(provider.id);
      await install(updated, false);
      await invalidateProviderSummaries(queryClient);
      onNotice(
        `${result.certified_count} of ${result.attempted_count} reviewed tuples passed server certification. Test the completed draft before activation.`
      );
    });
  }

  function nextPage() {
    const next = models.data?.page.nextCursor;
    if (!next) return;
    history = [...history, cursor];
    cursor = next;
  }

  function previousPage() {
    cursor = history.at(-1);
    history = history.slice(0, -1);
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
      disabled={Boolean(busy)}
      >{busy === 'detail-discover'
        ? 'Discovering…'
        : 'Run upstream discovery'}</button
    >
  </div>
  {#if current.kind === 'openai_compatible'}<details class="manual-fallback">
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
        disabled={Boolean(busy)}
        >{busy === 'detail-declare'
          ? 'Adding…'
          : 'Add identifiers for review'}</button
      >
    </details>{/if}
  {#if models.isError}<div class="inline-problem" role="alert">
      {providerDetailError(models.error)} The last loaded model page remains available
      below.
      <button
        class="button button-secondary"
        type="button"
        onclick={() => models.refetch()}>Retry</button
      >
    </div>{/if}
  {#if current.model_count === 0}<div class="empty-state">
      <p>No models have been discovered.</p>
    </div>
  {:else if models.isPending && !visibleModelPage}<div
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
          {#each modelPage.page.items as model (model.id)}<tr
              ><td
                ><strong>{model.display_name}</strong><br /><code
                  >{model.upstream_model}</code
                ></td
              ><td>
                <CapabilityReview
                  {model}
                  providerEtag={modelPage.provider.etag}
                  options={capabilityOptions.data?.capabilities ?? []}
                  optionsPending={capabilityOptions.isPending}
                  optionsError={capabilityOptions.isError}
                  disabled={Boolean(busy)}
                  {reloadVersion}
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
                    disabled={Boolean(busy) || !model.capabilities.length}
                    >{busy === `certify-${model.id}`
                      ? 'Server-certifying…'
                      : 'Server-certify capabilities'}</button
                  >
                  {#if certificationResults[model.id]}{@const result =
                      certificationResults[model.id]}<span
                      class:success={result.status === 'succeeded'}
                      class:warning={result.status !== 'succeeded'}
                      >{result.certified_count}/{result.attempted_count} certified</span
                    >
                    <ul class="certification-results">
                      {#each result.results.filter((item) => !item.succeeded) as item (`${item.operation}-${item.surface}-${item.mode}`)}<li
                        >
                          <code
                            >{item.operation}/{item.surface}/{item.mode}</code
                          >: {item.detail}
                        </li>{/each}
                    </ul>{/if}
                </div>
              </td></tr
            >{/each}
        </tbody>
      </table>
    </div>
    <CursorPagination
      page={history.length + 1}
      hasPrevious={history.length > 0}
      hasNext={Boolean(modelPage.page.nextCursor)}
      onPrevious={previousPage}
      onNext={nextPage}
      label="Provider model pages"
    />
  {/if}
  {#if !activationReady(current)}<p class="audit-note">
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
  .audit-note {
    color: var(--foreground-muted);
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
  .certification-results {
    width: 100%;
    margin: 0;
    padding-left: 1.25rem;
    color: var(--danger);
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
