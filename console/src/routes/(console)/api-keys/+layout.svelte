<script lang="ts">
  import { setContext } from 'svelte';
  import { emptyCursorHistory } from '$lib/api/pagination';
  import {
    apiKeyListStateContext,
    type ApiKeyListState
  } from '$lib/features/access/apiKeyListState';

  let { children } = $props();
  const listState = $state<ApiKeyListState>(emptyCursorHistory());

  setContext(apiKeyListStateContext, listState);
</script>

{@render children()}
