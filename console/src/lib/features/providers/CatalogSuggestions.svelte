<script lang="ts">
  import { createQuery } from '@tanstack/svelte-query';
  import { listCatalogSuggestions } from '#lib/features/providers/catalog.js';
  import type { Provider } from '#lib/features/providers/providers.js';
  import { providerKeys } from '#lib/features/providers/providerKeys.js';
  import {
    acceptableSuggestions,
    capabilityHints,
    changedFactLabels
  } from '#lib/features/providers/catalogSuggestions.js';
  import { lifecycleNotice } from '#lib/features/providers/provider-models/lifecycle.js';
  import { metadataFacts } from '#lib/features/providers/provider-models/modelMetadata.js';
  import { formatDate, stateLabel } from '#lib/format.js';
  import type { ProviderEditor } from './providerEditor.svelte';

  let {
    current,
    editor,
    locked
  }: { current: Provider; editor: ProviderEditor; locked: boolean } = $props();
  const editorView = $derived(editor.view);
  const canManage = $derived(editor.canManage);
  const busy = $derived(editorView.busy);
  const suggestions = createQuery(() => ({
    queryKey: providerKeys.catalogSuggestions(current.id, current.etag),
    queryFn: ({ signal }) => listCatalogSuggestions(current.id, signal)
  }));
  const acceptable = $derived(
    acceptableSuggestions(suggestions.data?.items ?? [])
  );
  let selected = $state<string[]>([]);

  async function accept() {
    const catalog = suggestions.data?.catalog;
    if (!canManage || locked || !catalog || !selected.length) return;
    if (await editor.acceptCatalog(catalog.sha256, selected)) selected = [];
  }
</script>

{#if suggestions.data?.items.length}
  {@const catalog = suggestions.data.catalog}
  <section class="catalog-suggestions" aria-labelledby="catalog-heading">
    <h3 id="catalog-heading">Reference catalog facts</h3>
    <p class="note">
      The signed reference catalog, published {formatDate(
        catalog.published_at
      )}, documents {suggestions.data.items.length} of these models under the
      <code>{suggestions.data.vendor_id}</code> vendor. Accepting stores its facts
      as operator facts tagged with the catalog's digest. It changes the provider
      configuration, so certified capabilities return to declared and must be certified
      again; the catalog certifies nothing.
    </p>
    <ul>
      {#each suggestions.data.items as item (item.model_id)}
        {@const notice = lifecycleNotice(item.lifecycle)}
        <li>
          <label class="suggestion-head">
            <input
              type="checkbox"
              value={item.upstream_model}
              bind:group={selected}
              disabled={!canManage ||
                locked ||
                !acceptable.includes(item) ||
                Boolean(busy)}
            />
            <span>
              <code>{item.upstream_model}</code>
              {#if item.catalog_model !== item.upstream_model}<small
                  >matched {item.catalog_model} by {stateLabel(
                    item.matched_by
                  )}</small
                >{/if}
            </span>
          </label>
          {#if item.conflict}<p class="conflict">
              Its stored facts attest a privacy declaration, so the catalog
              cannot replace their source. Edit the model's facts instead.
            </p>
          {:else if item.changes.length}<p class="changes">
              Would change: {changedFactLabels(item).join(', ')}.
            </p>
          {:else}<p class="changes">Its stored facts already match.</p>{/if}
          {#if capabilityHints(item).length}<p class="hints">
              Documented: {capabilityHints(item).join(', ')}. Hints only;
              certification decides.
            </p>{/if}
          {#if notice}<p
              class="lifecycle"
              class:danger={notice.tone === 'danger'}
            >
              {notice.text}
            </p>{/if}
          <details>
            <summary>Catalog facts</summary>
            <dl>
              {#each metadataFacts(item.facts) as fact (fact.label)}<div>
                  <dt>{fact.label}</dt>
                  <dd>{fact.value}</dd>
                </div>{/each}
              {#if item.facts.context_length}<div>
                  <dt>Context length</dt>
                  <dd>{item.facts.context_length}</dd>
                </div>{/if}
              {#if item.facts.max_output_tokens}<div>
                  <dt>Max output tokens</dt>
                  <dd>{item.facts.max_output_tokens}</dd>
                </div>{/if}
            </dl>
          </details>
        </li>
      {/each}
    </ul>
    {#if canManage}<button
        class="button button-secondary"
        type="button"
        disabled={locked ||
          editorView.refreshPending ||
          !selected.length ||
          Boolean(busy)}
        onclick={accept}
        >{busy === 'catalog-accept'
          ? 'Storing facts…'
          : `Accept facts for ${selected.length} model${selected.length === 1 ? '' : 's'}`}</button
      >{/if}
  </section>
{/if}

<style>
  .catalog-suggestions {
    display: grid;
    gap: 0.75rem;
    margin-top: 1rem;
    padding-top: 1rem;
    border-top: 1px solid var(--border);
  }
  h3 {
    margin: 0;
    font-size: var(--text-body);
  }
  .note,
  .changes,
  .hints {
    margin: 0;
    color: var(--foreground-muted);
    font-size: var(--text-body-sm);
  }
  ul {
    display: grid;
    gap: 0.75rem;
    margin: 0;
    padding: 0;
    list-style: none;
  }
  li {
    display: grid;
    gap: 0.3rem;
  }
  .suggestion-head {
    display: flex;
    align-items: baseline;
    gap: 0.5rem;
  }
  .suggestion-head small {
    margin-left: 0.4rem;
    color: var(--foreground-muted);
  }
  .conflict,
  .lifecycle {
    margin: 0;
    color: var(--warning);
    font-size: var(--text-body-sm);
  }
  .lifecycle.danger {
    color: var(--danger);
  }
  dl {
    display: grid;
    gap: 0.2rem;
    margin: 0.4rem 0 0;
    font-size: var(--text-caption);
  }
  dl div {
    display: flex;
    gap: 0.5rem;
  }
  dt {
    color: var(--foreground-muted);
  }
  dd {
    margin: 0;
  }
  button {
    justify-self: start;
  }
</style>
