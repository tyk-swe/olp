<script lang="ts">
  import { providerKeys } from '#lib/features/providers/providerKeys.js';
  import { usePrincipalLifetime } from '#lib/editing/mutationIntent.svelte.js';

  import { resolve } from '$app/paths';
  import { createQuery } from '@tanstack/svelte-query';
  import { errorMessage, isEtagMismatch } from '#lib/management/http.js';
  import CursorPagination from '#lib/shell/CursorPagination.svelte';
  import { formatDate } from '#lib/format.js';
  import {
    getProviderCapabilityOptions,
    type CapabilityCertification,
    type CapabilityDeclaration,
    type ProviderModel
  } from '#lib/features/providers/models.js';
  import type { Provider } from '#lib/features/providers/providers.js';
  import { declaresModels } from '#lib/features/providers/profiles.js';
  import { listProviderProfiles } from '#lib/features/providers/providerProfiles.js';
  import { cursorPaginationProps } from '#lib/listing/pagination.js';
  import CapabilityReview from '#lib/features/providers/CapabilityReview.svelte';
  import CatalogSuggestions from '#lib/features/providers/CatalogSuggestions.svelte';
  import {
    activationReady,
    DISABLED_EDIT_NOTE,
    providerDisabled
  } from '#lib/features/providers/providerEditor.js';
  import type { ProviderEditor } from './providerEditor.svelte';

  let { current, editor }: { current: Provider; editor: ProviderEditor } =
    $props();
  const editorView = $derived(editor.view);
  const busy = $derived(editorView.busy);
  const canManage = $derived(editor.canManage);
  const refreshPending = $derived(editorView.refreshPending);
  const modelSnapshot = $derived(editor.snapshot.data);
  const loading = $derived(editor.snapshot.isPending);
  const error = $derived(editor.snapshot.error);
  const reloadVersion = $derived(editorView.reloadVersion);
  const pageState = $derived(editor.modelPageState);
  let manualModelNames = $state('');
  let certificationResults = $state<Record<string, CapabilityCertification>>(
    {}
  );
  let previousProviderEtag = $state('');
  usePrincipalLifetime(() => {
    manualModelNames = '';
    certificationResults = {};
  });
  // Discovery, manual declaration and capability review all fail with the same
  // 409 on a disabled provider, so they stay locked until it is a draft again.
  const editingLocked = $derived(providerDisabled(current));
  const profiles = createQuery(() => ({
    queryKey: providerKeys.profiles(),
    queryFn: ({ signal }) => listProviderProfiles(signal)
  }));
  // A plugin profile without model discovery has no upstream model list; the
  // operator declares models.
  const declared = $derived(
    declaresModels(current.configuration, profiles.data)
  );

  const capabilityOptions = createQuery(() => ({
    queryKey: providerKeys.capabilityOptions(current.configuration.kind),
    queryFn: ({ signal }) =>
      getProviderCapabilityOptions(current.configuration.kind, signal)
  }));
  const visibleModelPage = $derived(modelSnapshot);
  $effect(() => {
    if (isEtagMismatch(error)) editor.markConflict();
  });

  $effect(() => {
    if (current.etag === previousProviderEtag) return;
    previousProviderEtag = current.etag;
    certificationResults = {};
  });

  async function discover() {
    if (!canManage || editingLocked || refreshPending) return;
    if (await editor.discoverModels()) certificationResults = {};
  }

  async function declareModels() {
    if (!canManage || editingLocked || refreshPending) return;
    const submitted = manualModelNames;
    if (await editor.declareModels(submitted)) {
      if (manualModelNames === submitted) manualModelNames = '';
      certificationResults = {};
    }
  }

  async function reviewModel(
    provider: Provider,
    model: ProviderModel,
    enabled: boolean,
    capabilities: CapabilityDeclaration[]
  ) {
    const confirmed = await editor.reviewModel(
      provider,
      model.id,
      enabled,
      capabilities
    );
    if (confirmed) certificationResults = {};
    return Boolean(confirmed);
  }

  async function certifyModel(provider: Provider, modelId: string) {
    const result = await editor.certifyModel(provider, modelId);
    if (result)
      certificationResults = { ...certificationResults, [modelId]: result };
  }
</script>

<section class="card editor models" aria-labelledby="models-heading">
  {#if refreshPending}<p class="inline-problem" role="status">
      The model change succeeded. Refresh the view before another change.
      <button
        type="button"
        class="button button-secondary"
        disabled={Boolean(busy)}
        onclick={editor.retryRefresh}>Retry refresh</button
      >
    </p>{/if}
  <div class="section-heading">
    <div>
      <p class="eyebrow">Discovery</p>
      <h2 id="models-heading">Models and capabilities</h2>
    </div>

    <a class="button button-secondary" href={resolve('models')}
      >Inventory view</a
    >
  </div>
  <div class="discovery-row">
    <p class="muted">
      {#if declared}This plugin profile declares no upstream model list.
        Rechecking probes every declared model; declare further models below.{:else}Refresh
        the inventory from the upstream model-list API. Existing capability
        certification is reconciled server-side.{/if}
    </p>
    <button
      class="button button-secondary"
      type="button"
      onclick={discover}
      disabled={!canManage || Boolean(busy) || refreshPending || editingLocked}
    >
      {busy === 'detail-discover'
        ? 'Discovering…'
        : declared
          ? 'Recheck declared models'
          : 'Run upstream discovery'}
    </button>
  </div>
  {#if editingLocked}<p class="locked-note">{DISABLED_EDIT_NOTE}</p>{/if}
  <CatalogSuggestions {current} {editor} locked={editingLocked} />
  {#if canManage && !editingLocked && (declared || current.configuration.kind === 'openai_compatible')}<details
      class="manual-fallback"
      open={declared}
    >
      <summary>Manual model identifiers</summary>
      <p>
        {declared
          ? 'Declare the upstream models this plugin provider serves.'
          : 'Use only if this compatible endpoint has no list API.'} Models remain
        disabled until capability review.
      </p>
      <div class="form-field">
        <label for="manual-models-detail">Upstream model identifiers</label
        ><textarea
          id="manual-models-detail"
          bind:value={manualModelNames}
          placeholder="model-a
model-b"></textarea>
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
      {errorMessage(error)} The last loaded model page remains available below.
      <button
        class="button button-secondary"
        type="button"
        onclick={() => editor.snapshot.refetch()}>Retry</button
      >
    </div>
  {/if}

  {#if current.model_count === 0}
    <div class="empty-state"><p>No models have been discovered.</p></div>
  {:else if loading && !visibleModelPage}
    <div class="loading-state" role="status">Loading models…</div>
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
                  disabled={!canManage ||
                    Boolean(busy) ||
                    refreshPending ||
                    editingLocked}
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
                      refreshPending ||
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
    padding: 1.5rem;
  }
  h2 {
    margin: 0 0 0.75rem;
    font-size: 1rem;
    font-weight: 500;
    letter-spacing: -0.02em;
  }
  .muted,
  .audit-note,
  .locked-note {
    color: var(--foreground-muted);
  }
  .locked-note {
    margin: 1rem 0 0;
    font-size: var(--text-body-sm);
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
    border-top: 1px solid var(--border-hairline);
    color: var(--foreground-muted);
    font-size: var(--text-body-sm);
  }
  .discovered {
    color: var(--foreground-muted);
    font-size: var(--text-caption);
  }
  .models {
    max-width: none;
  }
  .manual-fallback {
    margin-top: 1rem;
    padding: 0.75rem;
    border: 0;
    border-radius: var(--radius-control);
    background: var(--surface-raised);
  }
  .manual-fallback summary {
    min-height: 2.75rem;
    font-weight: 500;
  }
  .manual-fallback p {
    color: var(--foreground-muted);
    font-size: var(--text-body-sm);
  }
  .manual-fallback textarea {
    min-height: 5rem;
  }
  code {
    font-size: var(--text-caption);
  }
  @media (max-width: 42rem) {
    .discovery-row {
      display: grid;
    }
  }
</style>
