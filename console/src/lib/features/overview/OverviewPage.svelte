<script lang="ts">
  import Overview from '$lib/features/overview/Overview.svelte';
  import ControlOverview from '$lib/features/access/ControlOverview.svelte';
  import { useServiceCapabilities } from '$lib/features/access/session/serviceCapabilities.svelte';
  import { errorMessage } from '$lib/api/http';
  const services = useServiceCapabilities();
</script>

{#if services.pending}
  <div class="loading-state" role="status">Loading installation…</div>
{:else if services.error}
  <div class="inline-problem" role="alert">
    {errorMessage(services.error)}
    <button class="button button-secondary" onclick={() => services.retry()}
      >Retry</button
    >
  </div>
{:else if services.gatewayAvailable}
  <Overview controlConnected />
{:else}
  <ControlOverview />
{/if}
