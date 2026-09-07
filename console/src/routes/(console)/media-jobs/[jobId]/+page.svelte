<script lang="ts">
  import { page } from '$app/state';
  import MediaJobsPage from '$lib/features/media/MediaJobsPage.svelte';
  import { mediaJobList } from '$lib/features/media/mediaJobListState';

  let listState = mediaJobList.get();
</script>

<MediaJobsPage jobId={page.params.jobId} bind:listState />
