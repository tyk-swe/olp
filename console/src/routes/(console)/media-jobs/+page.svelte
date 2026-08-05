<script lang="ts">
  import { getContext } from 'svelte';
  import MediaJobsPage from '$lib/features/operations/media-jobs/MediaJobsPage.svelte';
  import {
    mediaJobListStateContext,
    type MediaJobListState
  } from '$lib/features/operations/media-jobs/mediaJobListState';

  let listState = getContext<MediaJobListState>(mediaJobListStateContext);
</script>

<MediaJobsPage bind:listState />
