<script lang="ts">
  import Fields from '../route-groups/GroupFields.svelte';
  import { groupError, groupForm, groupInput } from '../route-groups/policy';
  import {
    getProjectRouteGroups,
    putProjectRouteGroups,
    type ProjectRouteGroups
  } from './api';
  import ProjectPolicyEditor from './ProjectPolicyEditor.svelte';
  import { projectKeys } from './projectKeys';

  let {
    projectId,
    editable = true
  }: { projectId: string; editable?: boolean } = $props();
</script>

<ProjectPolicyEditor
  name="route groups"
  savedMessage="Route groups saved."
  help="Manage named route sets for keys in this project. Membership changes update every key that references a group."
  queryKey={projectKeys.routeGroups(projectId)}
  fetch={(signal) => getProjectRouteGroups(projectId, signal)}
  save={(etag, input) => putProjectRouteGroups(projectId, etag, input)}
  toForm={(doc?: ProjectRouteGroups) => groupForm(doc?.groups)}
  toInput={groupInput}
  validate={groupError}
  fields={Fields}
  prefix={`project-${projectId}-groups`}
  {editable}
/>
