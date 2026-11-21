<script lang="ts">
  import { page } from '$app/state';
  import PageTitle from '#lib/shell/branding/PageTitle.svelte';
  import UsagePage from '#lib/features/usage/UsagePage.svelte';
  import UsageSpendComparison from '#lib/features/usage/UsageSpendComparison.svelte';
  import {
    defaultUsageState,
    readUsageState
  } from '#lib/features/usage/usageState.js';

  let comparing = $state(false);
</script>

<PageTitle title="Usage" />
<button
  class="button button-secondary"
  type="button"
  aria-pressed={comparing}
  onclick={() => (comparing = !comparing)}>Compare with previous period</button
>

{#if comparing}
  <UsageSpendComparison
    filters={readUsageState(page.url.searchParams, defaultUsageState()).filters}
  />
{:else}
  <UsagePage />
{/if}
