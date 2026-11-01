<script lang="ts">
  import { page } from '$app/state';
  import TaskDetailPage from '$lib/features/agents/TaskDetailPage.svelte';
</script>

<TaskDetailPage taskID={page.params.taskId ?? ''} />
