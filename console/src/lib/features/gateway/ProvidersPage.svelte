<script lang="ts">
  import ProviderDetail from './ProviderDetail.svelte';
  import ProviderList from './ProviderList.svelte';
  import ProviderWizard from './ProviderWizard.svelte';

  let { path }: { path: string } = $props();

  const segments = $derived(path.split('/').filter(Boolean));
  const providerId = $derived(segments[1] && segments[1] !== 'new' ? segments[1] : '');
  const isNew = $derived(segments[1] === 'new');
  let providerCursor = $state<string | undefined>();
  let providerHistory = $state<Array<string | undefined>>([]);

  function setProviderCursor(cursor: string | undefined) {
    providerCursor = cursor;
  }

  function setProviderHistory(history: Array<string | undefined>) {
    providerHistory = history;
  }
</script>

<svelte:head><title>Providers · OpenLLMProxy</title></svelte:head>

{#if isNew}
  <ProviderWizard />
{:else if providerId}
  <ProviderDetail {providerId} />
{:else}
  <ProviderList {providerCursor} {providerHistory} onProviderCursorChange={setProviderCursor} onProviderHistoryChange={setProviderHistory} />
{/if}
