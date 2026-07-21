<script lang="ts">
  import { setContext } from 'svelte';
  import {
    apiKeyListStateContext,
    type ApiKeyListState
  } from '$lib/features/access/apiKeyListState';

  let { children } = $props();
  const listState = $state<ApiKeyListState>({ cursor: undefined, history: [] });

  setContext(apiKeyListStateContext, listState);
</script>

{@render children()}
