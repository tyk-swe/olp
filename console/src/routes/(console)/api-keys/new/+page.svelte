<script lang="ts">
  import { getContext } from 'svelte';
  import ApiKeysPage from '$lib/features/access/ApiKeysPage.svelte';
  import {
    apiKeyListStateContext,
    type ApiKeyListState
  } from '$lib/features/access/apiKeyListState';

  let listState = getContext<ApiKeyListState>(apiKeyListStateContext);
</script>

<ApiKeysPage isNew bind:listState />
