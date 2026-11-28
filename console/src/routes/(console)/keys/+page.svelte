<script lang="ts">
  import ApiKeysPage from '#lib/features/access/api-keys/ApiKeysPage.svelte';
  import { apiKeyList } from '#lib/features/access/api-keys/apiKeyListState.js';
  import { RoutingPolicyEditor } from '#lib/features/routes/index.js';

  let listState = apiKeyList.get();
</script>

<ApiKeysPage bind:listState {RoutingPolicyEditor} />
