<script lang="ts">
  import { QueryClient, setQueryClientContext } from '@tanstack/svelte-query';
  import { onMount, untrack } from 'svelte';

  let { setup }: { setup: () => void } = $props();

  const client = new QueryClient();
  setQueryClientContext(client);
  onMount(() => {
    client.mount();
    return () => client.unmount();
  });
  untrack(() => setup());
</script>
