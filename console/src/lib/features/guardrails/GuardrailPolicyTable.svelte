<script lang="ts">
  import ConfirmAction from '#lib/components/ConfirmAction.svelte';
  import EmptyState from '#lib/components/EmptyState.svelte';
  import ScrollTable from '#lib/components/ScrollTable.svelte';
  import type { Policy } from './api';

  let {
    pending,
    items,
    canRetire,
    onEdit,
    onRetire,
    onRetired
  }: {
    pending: boolean;
    items: Policy[];
    canRetire: boolean;
    onEdit: (value: Policy | null) => void;
    onRetire: (value: Policy, requestId: string) => Promise<unknown>;
    onRetired: (value: Policy) => Promise<void>;
  } = $props();
</script>

{#if !pending && !items.length}<EmptyState title="No policies yet"
    >Create a policy from one or more checks.</EmptyState
  >{:else}
  <ScrollTable label="Policies">
    <table class="data-table">
      <thead
        ><tr
          ><th scope="col">Name</th><th scope="col">Revision</th><th scope="col"
            ><span class="sr-only">Actions</span></th
          ></tr
        ></thead
      >
      <tbody>
        {#each items as value (value.policy_id)}<tr>
            <td
              ><button class="text-button" onclick={() => onEdit(value)}
                >{value.name}</button
              ></td
            >
            <td>{value.revision}</td>
            <td
              >{#if !value.retired_at && canRetire}<ConfirmAction
                  danger
                  label="Retire policy"
                  description={`Retire ${value.name}`}
                  identity={`${value.policy_id}:${value.etag}`}
                  run={(requestId) => onRetire(value, requestId)}
                  done={() => onRetired(value)}
                />{/if}</td
            >
          </tr>{/each}
      </tbody>
    </table>
  </ScrollTable>
{/if}
