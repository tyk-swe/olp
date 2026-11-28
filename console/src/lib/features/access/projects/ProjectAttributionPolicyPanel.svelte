<script lang="ts">
  import Fields from '../attribution/AttributionFields.svelte';
  import {
    attributionError,
    attributionForm,
    attributionInput
  } from '../attribution/policy';
  import {
    getProjectAttributionPolicy,
    putProjectAttributionPolicy,
    type ProjectAttributionPolicy
  } from './api';
  import { projectKeys } from '#lib/domain/projects/projectKeys.js';
  import ProjectPolicyEditor from './ProjectPolicyEditor.svelte';

  let {
    projectId,
    editable = true
  }: { projectId: string; editable?: boolean } = $props();
</script>

<ProjectPolicyEditor
  name="attribution policy"
  savedMessage="Attribution policy saved."
  help="Labels apply to every inference key in this project. Pinned values cannot be overridden by keys or callers. Clear all fields to remove the project policy."
  queryKey={projectKeys.attributionPolicy(projectId)}
  fetch={(signal) => getProjectAttributionPolicy(projectId, signal)}
  save={(etag, input) => putProjectAttributionPolicy(projectId, etag, input)}
  toForm={(doc?: ProjectAttributionPolicy) => attributionForm(doc?.policy)}
  toInput={attributionInput}
  validate={attributionError}
  fields={Fields}
  prefix={`project-${projectId}-attribution`}
  {editable}
/>
