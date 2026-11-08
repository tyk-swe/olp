<script lang="ts">
  import ScrollTable from '#lib/components/ScrollTable.svelte';
  import { createQuery } from '@tanstack/svelte-query';
  import { errorMessage } from '#lib/management/http.js';
  import { listExposures } from './api';
  import { agentKeys } from './agentKeys';
  let key = $state('');
  let filter = $state('');
  let cursor = $state<string | undefined>();
  const exposure = createQuery(() => ({
    queryKey: agentKeys.exposures(filter, cursor),
    queryFn: () => listExposures(filter, cursor)
  }));
</script>

<svelte:head><title>Provisional exposure · OpenLLMProxy</title></svelte:head>
<form
  class="filter-bar"
  onsubmit={(event) => {
    event.preventDefault();
    filter = key.trim();
    cursor = undefined;
  }}
>
  <label class="form-field"
    >API key UUID (optional) <input
      bind:value={key}
      pattern="[a-f0-9]{8}(-[a-f0-9]{4}){3}-[a-f0-9]{12}"
    /></label
  ><button class="button button-secondary" type="submit">Filter</button>
</form>
{#if exposure.error}<p class="inline-problem" role="alert">
    {errorMessage(exposure.error)}
  </p>{:else if exposure.isPending}<div class="loading-state" role="status">
    Loading exposure…
  </div>{:else if !exposure.data?.items.length}<div class="card empty-state">
    No execution debit records.
  </div>{:else}<ScrollTable label="Provisional exposure">
    <table class="data-table">
      <thead
        ><tr
          ><th scope="col">Dispatch / API key</th><th scope="col">State</th><th
            scope="col">Currency</th
          ><th scope="col">Provisional cost / charge</th><th scope="col"
            >Observed cost / charge</th
          ><th scope="col">Bound violation</th></tr
        ></thead
      ><tbody
        >{#each exposure.data?.items ?? [] as value (value.id)}<tr
            ><td
              ><code>{value.id}</code><br /><code>{value.api_key_id}</code></td
            ><td>{value.state}</td><td>{value.currency}</td><td
              >{value.remaining_cost} / {value.remaining_charge}</td
            ><td>{value.observed_cost} / {value.observed_charge}</td><td
              >{value.bound_violation ? 'Yes, investigate' : 'No'}</td
            ></tr
          >{/each}</tbody
      >
    </table>
  </ScrollTable>{/if}
<button
  class="button button-secondary"
  type="button"
  disabled={!cursor || exposure.isFetching}
  onclick={() => (cursor = undefined)}>Newest</button
><button
  class="button button-secondary"
  type="button"
  disabled={!exposure.data?.nextCursor || exposure.isFetching}
  onclick={() => (cursor = exposure.data?.nextCursor ?? undefined)}
  >Older</button
><button
  class="button button-secondary"
  type="button"
  disabled={exposure.isFetching}
  onclick={() => exposure.refetch()}>Refresh</button
>
