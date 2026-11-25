<script lang="ts">
  import Fields from './AttributionBudgetFields.svelte';
  import {
    budgetError,
    budgetForm,
    budgetInput
  } from './attributionBudgetPolicy';
  import {
    getProjectAttributionBudgets,
    putProjectAttributionBudgets,
    type ProjectAttributionBudgets
  } from './api';
  import ProjectPolicyEditor from './ProjectPolicyEditor.svelte';
  import { projectKeys } from './projectKeys';

  let {
    projectId,
    editable = true
  }: { projectId: string; editable?: boolean } = $props();
</script>

<ProjectPolicyEditor
  name="attribution budgets"
  savedMessage="Attribution budgets saved."
  help="All matching pairs are enforced alongside other budgets. Clearing a cap preserves current spending. Calendar windows follow the installation time zone."
  queryKey={projectKeys.attributionBudgets(projectId)}
  fetch={(signal) => getProjectAttributionBudgets(projectId, signal)}
  save={(etag, input) => putProjectAttributionBudgets(projectId, etag, input)}
  toForm={(doc?: ProjectAttributionBudgets) => budgetForm(doc?.budgets)}
  toInput={budgetInput}
  validate={budgetError}
  fields={Fields}
  prefix={`project-${projectId}-budgets`}
  {editable}
>
  {#snippet extra(doc: ProjectAttributionBudgets)}
    {#if Object.keys(doc.usage).length}
      <details>
        <summary>Current attributed spending</summary>
        <ul>
          {#each Object.entries(doc.usage) as [label, values] (label)}
            {#each Object.entries(values) as [value, usage] (value)}
              <li>
                {label}={value}: ${usage.daily.accrued} today, ${usage.weekly
                  ?.accrued ?? '0'} this week, ${usage.monthly.accrued} this month.
                {usage.unpriced_attempts} unpriced attempts this month.
              </li>
            {/each}
          {/each}
        </ul>
      </details>
    {/if}
  {/snippet}
</ProjectPolicyEditor>
