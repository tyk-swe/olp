<script lang="ts">
  import { page } from '$app/state';
  import ProjectPoliciesPage from '#lib/features/access/projects/ProjectPoliciesPage.svelte';
</script>

{#key page.params.projectId}
  <ProjectPoliciesPage projectId={page.params.projectId ?? ''} />
{/key}
