<script lang="ts">
  import { emptyCursorHistory } from '$lib/api/pagination';
  import {
    setApiKeyListState,
    type ApiKeyListState
  } from '$lib/features/access/api-keys/apiKeyListState';

  let { children } = $props();
  const listState = $state<ApiKeyListState>(emptyCursorHistory());

  setApiKeyListState(listState);
</script>

{@render children()}
