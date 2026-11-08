<script lang="ts">
  import RequestsPage from '#lib/features/usage/history/RequestsPage.svelte';
  import { requestList } from '#lib/features/usage/history/requestListState.js';

  let listState = requestList.get();
</script>

<RequestsPage bind:listState />
