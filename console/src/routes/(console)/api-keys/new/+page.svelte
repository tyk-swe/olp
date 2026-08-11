<script lang="ts">
  import ApiKeysPage from '$lib/features/access/api-keys/ApiKeysPage.svelte';
  import { getApiKeyListState } from '$lib/features/access/api-keys/apiKeyListState';

  let listState = getApiKeyListState();
</script>

<ApiKeysPage isNew bind:listState />
