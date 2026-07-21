<script lang="ts">
  import { setContext } from 'svelte';
  import {
    providerPaginationContext,
    type ProviderPagination
  } from '$lib/features/gateway/providerPagination';

  let { children } = $props();
  let cursor = $state<string | undefined>();
  let history = $state<Array<string | undefined>>([]);

  const pagination: ProviderPagination = {
    get cursor() { return cursor; },
    get history() { return history; },
    setCursor(value) { cursor = value; },
    setHistory(value) { history = value; }
  };

  setContext(providerPaginationContext, pagination);
</script>

{@render children()}
