<script lang="ts">
  import ApiKeysPage from '$lib/features/access/api-keys/ApiKeysPage.svelte';
  import { apiKeyList } from '$lib/features/access/api-keys/apiKeyListState';

  let listState = apiKeyList.get();
</script>

<ApiKeysPage isNew bind:listState />
