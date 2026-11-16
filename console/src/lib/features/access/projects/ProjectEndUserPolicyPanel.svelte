<script lang="ts">
  import Fields from '../end-users/PolicyFields.svelte';
  import { policyError, policyForm, policyInput } from '../end-users/policy';
  import {
    getProjectEndUserPolicy,
    putProjectEndUserPolicy,
    type ProjectEndUserPolicy
  } from './api';
  import ProjectPolicyEditor from './ProjectPolicyEditor.svelte';
  import { projectKeys } from './projectKeys';

  let {
    projectId,
    editable = true
  }: { projectId: string; editable?: boolean } = $props();
</script>

<ProjectPolicyEditor
  name="end-user policy"
  savedMessage="End-user policy saved."
  help="Project limits are shared across its keys for the same end-user digest. Every inference key must select an identifier source while this policy is enabled."
  queryKey={projectKeys.endUserPolicy(projectId)}
  fetch={(signal) => getProjectEndUserPolicy(projectId, signal)}
  save={(etag, input) => putProjectEndUserPolicy(projectId, etag, input)}
  toForm={(doc?: ProjectEndUserPolicy) => policyForm(doc?.policy)}
  toInput={policyInput}
  validate={policyError}
  fields={Fields}
  prefix={`project-${projectId}-end-user`}
  {editable}
/>
