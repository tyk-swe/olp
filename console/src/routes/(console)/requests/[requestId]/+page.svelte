<script lang="ts">
  import { page } from '$app/state';
  import RequestsPage from '$lib/features/usage/history/RequestsPage.svelte';
  import { requestList } from '$lib/features/usage/history/requestListState';

  let listState = requestList.get();
</script>

<RequestsPage requestId={page.params.requestId} bind:listState />
