<script lang="ts">
  import RequestsPage from '$lib/features/operations/requests/RequestsPage.svelte';
  import { requestList } from '$lib/features/operations/requests/requestListState';

  let listState = requestList.get();
</script>

<RequestsPage bind:listState />
