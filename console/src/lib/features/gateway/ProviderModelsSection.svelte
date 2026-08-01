<script lang="ts">
  import { resolve } from '$app/paths';
  import CursorPagination from '$lib/components/CursorPagination.svelte';
  import type {
    CapabilityCertification,
    CapabilityDeclaration,
    Provider,
    ProviderCapabilityOptions,
    ProviderModel
  } from '$lib/api/management/providers';
  import type { CursorPage } from '$lib/api/management/shared';
  import CapabilityReview from './CapabilityReview.svelte';
  import { activationReady } from './providerEditor';

  type CoordinatedModelPage = {
    page: CursorPage<ProviderModel>;
    provider: Provider;
  };

  let {
    current,
    visibleModelPage,
    modelPending,
    modelError,
    modelErrorMessage,
    capabilityOptions,
    capabilityOptionsPending,
    capabilityOptionsError,
    manualModelNames = $bindable(),
    certificationResults,
    reloadVersion,
    busy,
    page,
    hasPrevious,
    onRetry,
    onDiscover,
    onDeclare,
    onReview,
    onCertify,
    onPrevious,
    onNext
  }: {
    current: Provider;
    visibleModelPage: CoordinatedModelPage | undefined;
    modelPending: boolean;
    modelError: boolean;
    modelErrorMessage: string;
    capabilityOptions: ProviderCapabilityOptions['capabilities'];
    capabilityOptionsPending: boolean;
    capabilityOptionsError: boolean;
    manualModelNames: string;
    certificationResults: Record<string, CapabilityCertification>;
    reloadVersion: number;
    busy: string;
    page: number;
    hasPrevious: boolean;
    onRetry: () => void;
    onDiscover: () => void;
    onDeclare: () => void;
    onReview: (
      provider: Provider,
      model: ProviderModel,
      enabled: boolean,
      capabilities: CapabilityDeclaration[]
    ) => Promise<boolean>;
    onCertify: (provider: Provider, modelId: string) => void;
    onPrevious: () => void;
    onNext: () => void;
  } = $props();
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
      onclick={onDiscover}
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
          placeholder="model-a&#10;model-b"
        ></textarea>
      </div>
      <button
        class="button button-secondary"
        type="button"
        onclick={onDeclare}
        disabled={Boolean(busy)}
        >{busy === 'detail-declare'
          ? 'Adding…'
          : 'Add identifiers for review'}</button
      >
    </details>{/if}
  {#if modelError}<div class="inline-problem" role="alert">
      {modelErrorMessage} The last loaded model page remains available below.
      <button class="button button-secondary" type="button" onclick={onRetry}
        >Retry</button
      >
    </div>{/if}
  {#if current.model_count === 0}<div class="empty-state">
      <p>No models have been discovered.</p>
    </div>
  {:else if modelPending && !visibleModelPage}<div
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
                  options={capabilityOptions}
                  optionsPending={capabilityOptionsPending}
                  optionsError={capabilityOptionsError}
                  disabled={Boolean(busy)}
                  {reloadVersion}
                  onSave={(enabled, capabilities, providerEtag) =>
                    onReview(
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
                    onclick={() => onCertify(modelPage.provider, model.id)}
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
      {page}
      {hasPrevious}
      hasNext={Boolean(modelPage.page.nextCursor)}
      {onPrevious}
      {onNext}
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
