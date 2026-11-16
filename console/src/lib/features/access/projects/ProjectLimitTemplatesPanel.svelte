<script lang="ts">
  import Fields from '../limit-templates/TemplateFields.svelte';
  import {
    templateError,
    templateForm,
    templateInput
  } from '../limit-templates/policy';
  import {
    getProjectLimitTemplates,
    putProjectLimitTemplates,
    type ProjectLimitTemplates
  } from './api';
  import ProjectPolicyEditor from './ProjectPolicyEditor.svelte';
  import { projectKeys } from './projectKeys';

  let {
    projectId,
    editable = true
  }: { projectId: string; editable?: boolean } = $props();
</script>

<ProjectPolicyEditor
  name="limit templates"
  savedMessage="Limit templates saved."
  help="Manage shared limit ceilings for this project. Changes apply to every referencing key, end-user policy and budget group."
  queryKey={projectKeys.limitTemplates(projectId)}
  fetch={(signal) => getProjectLimitTemplates(projectId, signal)}
  save={(etag, input) => putProjectLimitTemplates(projectId, etag, input)}
  toForm={(doc?: ProjectLimitTemplates) => templateForm(doc?.templates)}
  toInput={templateInput}
  validate={templateError}
  fields={Fields}
  prefix={`project-${projectId}-templates`}
  {editable}
/>
