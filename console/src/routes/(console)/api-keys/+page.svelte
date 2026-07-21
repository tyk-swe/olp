<script lang="ts">
  import { getContext } from 'svelte';
  import ApiKeysPage from '$lib/features/access/ApiKeysPage.svelte';
  import {
    apiKeyListStateContext,
    type ApiKeyListState
  } from '$lib/features/access/apiKeyListState';

  const listState = getContext<ApiKeyListState>(apiKeyListStateContext);
</script>

<ApiKeysPage {listState} />
