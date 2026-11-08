<script lang="ts">
  import { page } from '$app/state';
  import ApiKeysPage from '#lib/features/access/api-keys/ApiKeysPage.svelte';
  import { apiKeyList } from '#lib/features/access/api-keys/apiKeyListState.js';

  let listState = apiKeyList.get();
</script>

{#key page.params.keyId}
  <ApiKeysPage keyId={page.params.keyId} bind:listState />
{/key}
