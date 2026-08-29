<script lang="ts">
  import { page } from '$app/state';
  import RequestsPage from '$lib/features/operations/requests/RequestsPage.svelte';
  import { requestList } from '$lib/features/operations/requests/requestListState';

  let listState = requestList.get();
</script>

<RequestsPage requestId={page.params.requestId} bind:listState />
