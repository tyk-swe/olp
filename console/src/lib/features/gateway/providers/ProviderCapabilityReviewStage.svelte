<script lang="ts">
  import type {
    CapabilityCertification,
    CapabilityDeclaration,
    ProviderModel
  } from '$lib/api/management/providerModels';
  import type { Provider } from '$lib/api/management/providers';
  import {
    cursorPaginationProps,
    type CursorHistory
  } from '$lib/lists/pagination';
  import CursorPagination from '$lib/components/CursorPagination.svelte';
  import { formatDate } from '$lib/format';
  import CapabilityReview from './CapabilityReview.svelte';

  let {
    provider,
    models,
    modelsPending,
    modelsError,
    capabilityOptions,
    optionsPending,
    optionsError,
    busy,
    reloadVersion,
    certificationResults,
    pagination,
    nextCursor,
    onSave,
    onCertify,
    onRetryModels
  }: {
    provider: Provider;
    models: ProviderModel[];
    modelsPending: boolean;
    modelsError: boolean;
    capabilityOptions: CapabilityDeclaration[];
    optionsPending: boolean;
    optionsError: boolean;
    busy: string;
    reloadVersion: number;
    certificationResults: Readonly<Record<string, CapabilityCertification>>;
    pagination: CursorHistory;
    nextCursor: string | null | undefined;
    onSave: (
      modelId: string,
      enabled: boolean,
      capabilities: CapabilityDeclaration[],
      providerEtag: string
    ) => Promise<boolean>;
    onCertify: (modelId: string) => void | Promise<void>;
    onRetryModels: () => void;
  } = $props();

  const disabled = $derived(Boolean(busy));
</script>

<p class="eyebrow">Capability review</p>
<h2 id="capability-heading">Review model capabilities</h2>
<p class="description">
  Operator review is recorded as <code>declared</code>. Every native and
  compatible capability tuple must receive server-owned certification for this
  exact draft.
</p>
{#if modelsError}
  <div class="inline-problem" role="alert">
    The discovered models could not be loaded. <button
      class="button button-secondary"
      type="button"
      onclick={onRetryModels}>Retry</button
    >
  </div>
{:else if modelsPending}
  <div class="loading-state" role="status">Loading discovered models…</div>
{:else if !models.length && pagination.history.length === 0}
  <div class="empty-state">
    <p>
      No models are available for review. Run discovery again, or declare
      upstream model identifiers.
    </p>
  </div>
{:else}
  <div class="table-shell">
    <table class="data-table">
      <thead><tr><th>Model</th><th>Explicit capability review</th></tr></thead>
      <tbody>
        {#each models as model (model.id)}
          <tr>
            <td>
              <strong>{model.display_name}</strong><br />
              <code>{model.upstream_model}</code>
              {#if model.discovered_at}<br /><small class="discovered"
                  >Discovered {formatDate(model.discovered_at)}</small
                >{/if}
            </td>
            <td>
              <CapabilityReview
                {model}
                providerEtag={provider.etag}
                options={capabilityOptions}
                {optionsPending}
                {optionsError}
                {disabled}
                {reloadVersion}
                certification={certificationResults[model.id]}
                onSave={(enabled, capabilities, providerEtag) =>
                  onSave(model.id, enabled, capabilities, providerEtag)}
              />
              <div class="certification-action">
                <button
                  class="button button-secondary"
                  type="button"
                  onclick={() => onCertify(model.id)}
                  disabled={disabled || !model.capabilities.length}
                  >{busy === `certify-${model.id}`
                    ? 'Server-certifying…'
                    : 'Server-certify capabilities'}</button
                >
                {#if certificationResults[model.id]}
                  {@const certification = certificationResults[model.id]}
                  <span
                    class:success={certification.status === 'succeeded'}
                    class:warning={certification.status !== 'succeeded'}
                    >{certification.certified_count}/{certification.attempted_count}
                    certified</span
                  >
                {/if}
              </div>
            </td>
          </tr>
        {/each}
      </tbody>
    </table>
  </div>
  <CursorPagination
    {...cursorPaginationProps(pagination, nextCursor)}
    label="Provider wizard model pages"
  />
{/if}

<style>
  h2 {
    margin: 0 0 0.85rem;
    font-size: 1.15rem;
    font-weight: 750;
    letter-spacing: -0.025em;
  }
  .description {
    color: var(--foreground-muted);
  }
  .discovered {
    color: var(--foreground-muted);
    font-size: 0.7rem;
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
  code {
    font:
      0.75rem 'JetBrains Mono Variable',
      monospace;
  }
</style>
