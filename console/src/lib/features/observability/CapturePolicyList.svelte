<script lang="ts">
  import EmptyState from '#lib/components/EmptyState.svelte';
  import ScrollTable from '#lib/components/ScrollTable.svelte';
  import { formatUTCDate } from '#lib/format.js';
  import type { CapturePolicy } from '#lib/features/observability/api.js';

  let {
    canCreate,
    onCreate,
    pending,
    failed,
    items,
    onRetry,
    canMutate,
    busy,
    onEdit,
    onToggle,
    onRemove,
    sinkOptionLabel
  }: {
    canCreate: boolean;
    onCreate: () => void;
    pending: boolean;
    failed: boolean;
    items: CapturePolicy[];
    onRetry: () => void;
    canMutate: (projectId: string | null) => boolean;
    busy: boolean;
    onEdit: (policy: CapturePolicy) => void;
    onToggle: (policy: CapturePolicy) => void;
    onRemove: (policy: CapturePolicy) => void;
    sinkOptionLabel: (id: string) => string;
  } = $props();
</script>

{#if canCreate}<div class="toolbar">
    <p class="section-help">
      Choose which prompts and responses are kept, for which projects, and where
      they go.
    </p>
    <div class="page-actions">
      <button class="button button-primary" type="button" onclick={onCreate}
        >New policy</button
      >
    </div>
  </div>{/if}

{#if pending}<span class="inline-status" role="status"
    >Loading capture policies…</span
  >
{:else if failed}<span class="inline-problem" role="alert"
    >Capture policies are unavailable.
    <button class="text-button" type="button" onclick={onRetry}>Retry</button
    ></span
  >
{:else if !items.length}<EmptyState title="No capture policies yet"
    >Prompts and responses are not stored unless a policy turns capture on.</EmptyState
  >
{:else}
  <ScrollTable label="Capture policies">
    <table class="data-table">
      <thead
        ><tr
          ><th scope="col">Scope</th><th scope="col">Sink</th><th scope="col"
            >Ratio</th
          ><th scope="col">Includes</th><th scope="col">Redactors</th><th
            scope="col">Enabled</th
          ><th scope="col">Expires</th><th scope="col"
            ><span class="sr-only">Actions</span></th
          ></tr
        ></thead
      >
      <tbody>
        {#each items as policy (policy.id)}
          <tr
            ><td
              >{policy.project_id
                ? 'Project'
                : 'Installation'}{policy.route_slug
                ? ` · ${policy.route_slug}`
                : ''}</td
            ><td>{sinkOptionLabel(policy.sink)}</td><td
              ><span class="mono">{policy.sample_ratio}</span></td
            ><td>{policy.include.join(', ')}</td><td>
              {#if policy.redact.length}
                <ol>
                  {#each policy.redactor_revisions ?? [] as pin (pin.revision_id)}<li
                    >
                      {pin.name} · revision {pin.revision}
                      <span class="mono">{pin.revision_id}</span>
                    </li>{/each}
                </ol>
              {:else}Unredacted{/if}
            </td><td>{policy.enabled ? 'Yes' : 'No'}</td><td>
              {#if policy.expires_at}
                <time datetime={policy.expires_at}
                  >{formatUTCDate(policy.expires_at)}</time
                >
              {:else}Never{/if}
            </td><td
              >{#if canMutate(policy.project_id)}<button
                  class="text-button"
                  type="button"
                  disabled={busy}
                  onclick={() => onEdit(policy)}>Edit</button
                ><button
                  class="text-button"
                  type="button"
                  onclick={() => onToggle(policy)}
                  >{policy.enabled ? 'Disable' : 'Enable'}</button
                ><button
                  class="text-button"
                  type="button"
                  onclick={() => onRemove(policy)}>Delete</button
                >{/if}</td
            ></tr
          >
        {/each}
      </tbody>
    </table>
  </ScrollTable>
{/if}
