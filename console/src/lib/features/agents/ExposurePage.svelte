<script lang="ts">
  import { createQuery } from '@tanstack/svelte-query';
  import { errorMessage } from '$lib/api/http';
  import { listExposures } from './api';
  let key = $state('');
  let filter = $state('');
  let cursor = $state<string | undefined>();
  const exposure = createQuery(() => ({
    queryKey: ['execution-debits', filter, cursor],
    queryFn: () => listExposures(filter, cursor)
  }));
</script>

<svelte:head><title>Provisional exposure · OpenLLMProxy</title></svelte:head>
<header>
  <p class="eyebrow">Budgets</p>
  <h1>Provisional exposure</h1>
  <p>
    Unresolved finite obligations remain reserved across restarts and budget
    resets. These bounds are not accrued spend or invoice charges. Observed
    amounts are already accounted usage; do not add them to billing again.
  </p>
</header>
<form
  onsubmit={(event) => {
    event.preventDefault();
    filter = key.trim();
    cursor = undefined;
  }}
>
  <label
    >API key UUID (optional) <input
      bind:value={key}
      pattern="[a-f0-9]{8}(-[a-f0-9]{4}){3}-[a-f0-9]{12}"
    /></label
  ><button type="submit">Filter</button>
</form>
{#if exposure.error}<p role="alert">
    {errorMessage(exposure.error)}
  </p>{:else if exposure.isPending}<p>
    Loading exposure…
  </p>{:else if !exposure.data?.items.length}<p>
    No execution debit records.
  </p>{:else}<table>
    <thead
      ><tr
        ><th>Dispatch / API key</th><th>State</th><th>Currency</th><th
          >Provisional cost / charge</th
        ><th>Observed cost / charge</th><th>Bound violation</th></tr
      ></thead
    ><tbody
      >{#each exposure.data?.items ?? [] as value (value.id)}<tr
          ><td><code>{value.id}</code><br /><code>{value.api_key_id}</code></td
          ><td>{value.state}</td><td>{value.currency}</td><td
            >{value.remaining_cost} / {value.remaining_charge}</td
          ><td>{value.observed_cost} / {value.observed_charge}</td><td
            >{value.bound_violation ? 'Yes — investigate' : 'No'}</td
          ></tr
        >{/each}</tbody
    >
  </table>{/if}
<button
  type="button"
  disabled={!cursor || exposure.isFetching}
  onclick={() => (cursor = undefined)}>Newest</button
><button
  type="button"
  disabled={!exposure.data?.nextCursor || exposure.isFetching}
  onclick={() => (cursor = exposure.data?.nextCursor ?? undefined)}
  >Older</button
><button
  type="button"
  disabled={exposure.isFetching}
  onclick={() => exposure.refetch()}>Refresh</button
>

<style>
  header,
  form {
    margin-bottom: 1rem;
  }
  table {
    width: 100%;
    text-align: left;
  }
  td,
  th {
    padding: 0.5rem;
  }
  code {
    overflow-wrap: anywhere;
  }
  button {
    margin: 0.5rem 0.5rem 0 0;
  }
</style>
