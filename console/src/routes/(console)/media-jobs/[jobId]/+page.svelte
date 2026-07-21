<script lang="ts">
  import { page } from '$app/state';
  import { getContext } from 'svelte';
  import MediaJobsPage from '$lib/features/operations/MediaJobsPage.svelte';
  import {
    mediaJobListStateContext,
    type MediaJobListState
  } from '$lib/features/operations/mediaJobListState';

  const listState = getContext<MediaJobListState>(mediaJobListStateContext);
</script>

<MediaJobsPage jobId={page.params.jobId} {listState} />
