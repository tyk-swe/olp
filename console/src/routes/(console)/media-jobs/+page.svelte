<script lang="ts">
  import { getContext } from 'svelte';
  import MediaJobsPage from '$lib/features/operations/MediaJobsPage.svelte';
  import {
    mediaJobListStateContext,
    type MediaJobListState
  } from '$lib/features/operations/mediaJobListState';

  let listState = getContext<MediaJobListState>(mediaJobListStateContext);
</script>

<MediaJobsPage bind:listState />
