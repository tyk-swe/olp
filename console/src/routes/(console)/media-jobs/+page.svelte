<script lang="ts">
  import MediaJobsPage from '$lib/features/media/MediaJobsPage.svelte';
  import { mediaJobList } from '$lib/features/media/mediaJobListState';

  let listState = mediaJobList.get();
</script>

<MediaJobsPage bind:listState />
