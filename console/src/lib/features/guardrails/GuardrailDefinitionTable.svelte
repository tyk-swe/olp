<script lang="ts">
  import ConfirmAction from '#lib/components/ConfirmAction.svelte';
  import EmptyState from '#lib/components/EmptyState.svelte';
  import ScrollTable from '#lib/components/ScrollTable.svelte';
  import StatusBadge from '#lib/components/StatusBadge.svelte';
  import type { Definition } from '#lib/domain/guardrails/api.js';

  let {
    pending,
    items,
    canRetire,
    onEdit,
    onRetire,
    onRetired
  }: {
    pending: boolean;
    items: Definition[];
    canRetire: boolean;
    onEdit: (value: Definition | null) => void;
    onRetire: (value: Definition, requestId: string) => Promise<unknown>;
    onRetired: (value: Definition) => Promise<void>;
  } = $props();
</script>

{#if !pending && !items.length}<EmptyState title="No checks yet">
    Create a check, then add it to a policy.
  </EmptyState>{:else}
  <ScrollTable label="Checks">
    <table class="data-table">
      <thead>
        <tr>
          <th scope="col">Name</th><th scope="col">Type</th><th scope="col">
            Revision
          </th><th scope="col"><span class="sr-only">Actions</span></th>
        </tr>
      </thead>
      <tbody>
        {#each items as value (value.id)}<tr>
            <td>
              <button class="text-button" onclick={() => onEdit(value)}>
                {value.name}
              </button>
            </td>
            <td class="mono">{value.type}</td>
            <td>
              {value.revision}
              <StatusBadge
                label={value.active_revision_id === value.revision_id
                  ? 'Active'
                  : 'Draft'}
                tone={value.active_revision_id === value.revision_id
                  ? 'success'
                  : 'warning'}
              />
            </td>
            <td>
              {#if !value.retired_at && canRetire}<ConfirmAction
                  danger
                  label="Retire check"
                  description={`Retire ${value.name}`}
                  identity={`${value.id}:${value.etag}`}
                  run={(requestId) => onRetire(value, requestId)}
                  done={() => onRetired(value)}
                />{/if}
            </td>
          </tr>{/each}
      </tbody>
    </table>
  </ScrollTable>
{/if}
