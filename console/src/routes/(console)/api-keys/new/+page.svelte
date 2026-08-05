<script lang="ts">
  import { getContext } from 'svelte';
  import ApiKeysPage from '$lib/features/access/api-keys/ApiKeysPage.svelte';
  import {
    apiKeyListStateContext,
    type ApiKeyListState
  } from '$lib/features/access/api-keys/apiKeyListState';

  let listState = getContext<ApiKeyListState>(apiKeyListStateContext);
</script>

<ApiKeysPage isNew bind:listState />
