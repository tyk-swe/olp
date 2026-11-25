<script lang="ts">
  import { createQuery } from '@tanstack/svelte-query';
  import { errorMessage } from '#lib/management/http.js';
  import {
    cursorPaginationProps,
    emptyCursorHistory,
    resetCursor
  } from '#lib/listing/pagination.js';
  import CursorPagination from '#lib/components/CursorPagination.svelte';
  import SecretDialog from '#lib/components/SecretDialog.svelte';
  import { providerKeys } from './providerKeys.js';
  import {
    getProviderRevision,
    listProviderRevisionModelPage
  } from './revisions.js';
  import { formatDate, stateLabel } from '#lib/format.js';

  let {
    providerId,
    viewed,
    onClose
  }: {
    providerId: string;
    viewed: { id: string; revision: number };
    onClose: () => void;
  } = $props();

  const modelPagination = $state(emptyCursorHistory());
  let viewedId = $state('');
  $effect(() => {
    if (viewed.id !== viewedId) {
      viewedId = viewed.id;
      resetCursor(modelPagination);
    }
  });
  const revisionDetail = createQuery(() => ({
    queryKey: providerKeys.revision(providerId, viewed.id),
    queryFn: ({ signal }) => getProviderRevision(providerId, viewed.id, signal)
  }));
  const revisionModels = createQuery(() => ({
    queryKey: providerKeys.revisionModels(
      providerId,
      viewed.id,
      modelPagination.cursor
    ),
    queryFn: ({ signal }) =>
      listProviderRevisionModelPage(
        providerId,
        viewed.id,
        modelPagination.cursor,
        signal
      )
  }));
</script>

<SecretDialog
  eyebrow="Immutable revision"
  title={`Revision ${viewed.revision}`}
  description="Configuration and model inventory captured when this revision was activated. Credential material is never returned."
  size="wide"
  {onClose}
>
  {#snippet children(close)}
    {#if revisionDetail.isPending}<p class="inline-status" role="status">
        Loading revision…
      </p>
    {:else if revisionDetail.isError}<div class="inline-problem" role="alert">
        {errorMessage(revisionDetail.error)}
        <button
          class="button button-secondary"
          type="button"
          onclick={() => revisionDetail.refetch()}
        >
          Retry
        </button>
      </div>
    {:else if revisionDetail.data}
      {@const detail = revisionDetail.data}
      <dl class="revision-fields">
        <div>
          <dt>Name</dt>
          <dd>{detail.name}</dd>
        </div>
        <div>
          <dt>Connector</dt>
          <dd>{stateLabel(detail.configuration.kind)}</dd>
        </div>
        <div>
          <dt>Authentication</dt>
          <dd>{detail.configuration.auth_mode}</dd>
        </div>
        <div>
          <dt>Endpoint</dt>
          <dd>{detail.configuration.endpoint ?? 'Connector default'}</dd>
        </div>
        <div>
          <dt>Cloud region</dt>
          <dd>{detail.configuration.cloud_region ?? 'Not set'}</dd>
        </div>
        <div>
          <dt>Cloud project</dt>
          <dd>{detail.configuration.cloud_project ?? 'Not set'}</dd>
        </div>
        <div>
          <dt>Deployment</dt>
          <dd>{detail.configuration.deployment ?? 'Not set'}</dd>
        </div>
        <div>
          <dt>API version</dt>
          <dd>{detail.configuration.api_version ?? 'Not set'}</dd>
        </div>
        <div>
          <dt>Models</dt>
          <dd>
            {detail.model_count} total · {detail.enabled_model_count} enabled
          </dd>
        </div>
        <div>
          <dt>Capabilities</dt>
          <dd>
            {detail.capability_count} total · {detail.certified_capability_count}
            certified
          </dd>
        </div>
        <div>
          <dt>Connector available</dt>
          <dd>{detail.connector_ready ? 'Yes' : 'No'}</dd>
        </div>
        <div>
          <dt>Credential metadata</dt>
          <dd>
            {detail.historical_credential_version == null
              ? 'Workload identity'
              : `Version ${detail.historical_credential_version}`}
          </dd>
        </div>
        <div>
          <dt>Activated</dt>
          <dd>
            {formatDate(detail.activated_at)} by
            <code>{detail.activated_by}</code>
          </dd>
        </div>
        <div>
          <dt>Source ETag</dt>
          <dd><code>{detail.source_etag}</code></dd>
        </div>
      </dl>
    {/if}

    <h3 class="revision-models-heading">Models and capabilities</h3>
    {#if revisionModels.isPending}<p class="inline-status" role="status">
        Loading revision models…
      </p>
    {:else if revisionModels.isError}<div class="inline-problem" role="alert">
        {errorMessage(revisionModels.error)}
        <button
          class="button button-secondary"
          type="button"
          onclick={() => revisionModels.refetch()}
        >
          Retry
        </button>
      </div>
    {:else if !revisionModels.data?.items.length && modelPagination.history.length === 0}
      <p class="muted">This revision carried no models.</p>
    {:else}
      <ul class="revision-models">
        {#each revisionModels.data?.items ?? [] as model (model.id)}<li>
            <div class="revision-model-name">
              <strong>{model.display_name}</strong>
              <code>{model.upstream_model}</code>
              <span class="badge {model.enabled ? 'success' : 'warning'}">
                {model.enabled ? 'enabled' : 'not enabled'}
              </span>
            </div>
            {#if model.discovered_at}<small>
                Discovered {formatDate(model.discovered_at)}
              </small>{/if}
            <div class="revision-capabilities">
              {#each model.capabilities as capability (`${capability.operation}-${capability.surface}-${capability.mode}`)}<span
                  class:certified={capability.source === 'certified'}
                >
                  <code>
                    {capability.operation}/{capability.surface}/{capability.mode}
                  </code>
                  · {capability.source}{#if capability.certified_at}
                    · {formatDate(capability.certified_at)}{/if}
                </span>{:else}<span class="muted">
                  No capabilities recorded.
                </span>{/each}
            </div>
          </li>{/each}
      </ul>
      <CursorPagination
        {...cursorPaginationProps(
          modelPagination,
          revisionModels.data?.nextCursor
        )}
        label="Revision model pages"
      />
    {/if}
    <div class="dialog-actions">
      <button
        class="button button-primary"
        type="button"
        data-autofocus
        onclick={close}
      >
        Close revision
      </button>
    </div>
  {/snippet}
</SecretDialog>

<style>
  .revision-fields {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: 0.65rem 1rem;
    margin: 0;
  }
  .revision-fields dt {
    color: var(--foreground-subtle);
    font-family: var(--font-mono);
    font-size: var(--text-caption);
    font-weight: 400;
    letter-spacing: -0.02em;
    text-transform: uppercase;
  }
  .revision-fields dd {
    margin: 0.15rem 0 0;
    overflow-wrap: anywhere;
  }
  .revision-models-heading {
    margin: 1.35rem 0 0.5rem;
    font-size: var(--text-body);
    font-weight: 500;
    letter-spacing: -0.02em;
  }
  .revision-models {
    display: grid;
    gap: 0.6rem;
    margin: 0;
    padding: 0;
    list-style: none;
  }
  .revision-models li {
    display: grid;
    gap: 0.3rem;
    padding: 0.75rem;
    border: 1px solid var(--border-hairline);
    border-radius: var(--radius-control);
    background: var(--canvas);
  }
  .revision-model-name {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 0.5rem;
  }
  .revision-capabilities {
    display: flex;
    flex-wrap: wrap;
    gap: 0.35rem;
  }
  .revision-capabilities span:not(.muted) {
    padding: 0.3rem 0.45rem;
    border: 1px solid var(--border);
    border-radius: var(--radius-control);
    color: var(--warning);
    font-family: var(--font-mono);
    font-size: var(--text-caption);
    font-weight: 400;
    letter-spacing: -0.02em;
    text-transform: uppercase;
  }
  .revision-capabilities span.certified {
    color: var(--success);
  }
  .dialog-actions {
    display: flex;
    justify-content: flex-end;
    margin-top: 1.25rem;
  }
  code {
    font-size: var(--text-caption);
  }
  @media (max-width: 42rem) {
    .revision-fields {
      grid-template-columns: 1fr;
    }
  }
</style>
