<script lang="ts">
  import { resolve } from '$app/paths';
  import ScrollTable from '#lib/components/ScrollTable.svelte';
  import { formatInteger, stateLabel } from '#lib/format.js';
  import { metadataFacts } from '#lib/features/providers/provider-models/modelMetadata.js';
  import { lifecycleNotice } from '#lib/features/providers/provider-models/lifecycle.js';
  import type { ProviderModelInventory } from '#lib/features/providers/models.js';

  let {
    inventory,
    canManage,
    busy,
    updating,
    eligibilityVersion,
    onToggle
  }: {
    inventory: ProviderModelInventory[];
    canManage: boolean;
    busy: boolean;
    updating: boolean;
    eligibilityVersion: number;
    onToggle: (entry: ProviderModelInventory, enabled: boolean) => void;
  } = $props();
</script>

{#if updating}
  <p class="updating" role="status">Updating…</p>
{/if}
<ScrollTable label="Provider models" busy={updating}>
  <table class="data-table">
    <thead
      ><tr
        ><th>Model</th><th>Provider</th><th>Context</th><th>Max output</th><th
          >Capability tuples / provenance</th
        ><th>Route eligibility</th></tr
      ></thead
    ><tbody>
      {#each inventory as entry (`${entry.provider_id}-${entry.model.id}`)}
        {@const facts = metadataFacts(entry.metadata)}
        {@const lifecycle = lifecycleNotice(entry.lifecycle)}
        <tr>
          <td
            ><strong>{entry.model.display_name}</strong><br /><code
              >{entry.model.upstream_model}</code
            >{#if lifecycle}<small
                class="lifecycle-notice"
                class:danger={lifecycle.tone === 'danger'}
                >{lifecycle.text}</small
              >{/if}{#if facts.length}<details class="model-facts">
                <summary>Upstream metadata</summary>
                <dl>
                  {#each facts as fact (fact.label)}<div>
                      <dt>{fact.label}</dt>
                      <dd>{fact.value}</dd>
                    </div>{/each}
                </dl>
              </details>
            {/if}
          </td>
          <td>
            <a href={resolve(`providers/${entry.provider_id}`)}
              >{entry.provider_name}</a
            >
            <br />
            <span class="badge">{stateLabel(entry.provider_kind)}</span>
          </td>
          <td>{formatInteger(entry.metadata.context_length)}</td>
          <td>{formatInteger(entry.metadata.max_output_tokens)}</td>
          <td
            ><div class="capabilities">
              {#each entry.model.capabilities as capability (`${capability.operation}-${capability.surface}-${capability.mode}`)}<span
                  class:success={capability.source === 'certified'}
                  class:accent={capability.source !== 'certified'}
                  class="badge"
                  ><strong>{capability.operation}</strong>
                  {capability.surface} · {capability.mode} · {capability.source}</span
                >{/each}
            </div></td
          >
          <td
            ><label class="eligibility"
              >{#key `${entry.model.id}:${eligibilityVersion}`}<input
                  type="checkbox"
                  checked={entry.model.enabled}
                  disabled={!canManage || busy || updating}
                  onchange={(event) =>
                    onToggle(entry, event.currentTarget.checked)}
                />{/key}<span
                >{entry.model.enabled ? 'Enabled' : 'Disabled'}</span
              ></label
            ></td
          >
        </tr>
      {/each}
    </tbody>
  </table>
</ScrollTable>

<style>
  code {
    font-size: var(--text-caption);
  }
  .model-facts {
    margin-top: 0.4rem;
    max-width: 22rem;
  }
  .model-facts summary {
    cursor: pointer;
    color: var(--foreground-muted);
    font-size: var(--text-caption);
  }
  .model-facts dl {
    display: grid;
    margin: 0.5rem 0 0;
    gap: 0.3rem;
  }
  .model-facts div {
    display: grid;
    grid-template-columns: minmax(0, 9rem) minmax(0, 1fr);
    gap: 0.5rem;
  }
  .model-facts dt {
    color: var(--foreground-muted);
    font-size: var(--text-caption);
  }
  .model-facts dd {
    margin: 0;
    font-family: var(--font-mono);
    font-size: var(--text-caption);
    overflow-wrap: anywhere;
  }
  td a {
    color: var(--foreground);
    font-weight: 400;
    text-decoration: underline;
    text-decoration-color: var(--border-strong);
    text-underline-offset: 4px;
    transition:
      color var(--motion),
      text-decoration-color var(--motion);
  }
  td a:hover {
    color: var(--foreground-hover);
    text-decoration-color: currentColor;
  }
  .capabilities {
    display: flex;
    max-width: 38rem;
    flex-wrap: wrap;
    gap: 0.35rem;
  }
  .capabilities .badge {
    gap: 0.25rem;
  }
  .eligibility {
    display: inline-flex;
    min-height: 2.75rem;
    align-items: center;
    gap: 0.5rem;
    font-weight: 500;
  }
  .lifecycle-notice {
    display: block;
    margin-top: 0.3rem;
    color: var(--warning);
    font-size: var(--text-caption);
  }
  .lifecycle-notice.danger {
    color: var(--danger);
  }
</style>
