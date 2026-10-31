<script lang="ts">
  import { goto } from '$app/navigation';
  import { resolve } from '$app/paths';
  import { page } from '$app/state';
  import { createQuery } from '@tanstack/svelte-query';
  import { errorMessage } from '$lib/api/http';
  import { listProjectMemberships } from '$lib/features/access/projects/api';
  import { projectKeys } from '$lib/features/access/projects/projectKeys';
  import {
    createInvestigation,
    type InvestigationEvidence,
    type InvestigationWrite
  } from './api';

  const initialProject = page.url.searchParams.get('project') ?? '';
  const initialRun = page.url.searchParams.get('run') ?? '';
  const requestID = crypto.randomUUID();
  let evidence = $state<InvestigationEvidence[]>(
    initialRun ? [{ kind: 'run', id: initialRun }] : []
  );
  let evidenceKind = $state<InvestigationEvidence['kind']>('run');
  let evidenceID = $state('');
  let evidenceError = $state('');
  let error = $state('');
  let saving = $state(false);
  const projects = createQuery(() => ({
    queryKey: projectKeys.memberships,
    queryFn: ({ signal }) => listProjectMemberships(signal),
    staleTime: 0,
    gcTime: 0,
    retry: false
  }));

  function addEvidence() {
    if (!evidenceID.trim()) {
      evidenceError = 'Enter an evidence ID.';
      return;
    }
    if (evidence.length >= 100) {
      evidenceError = 'An investigation can include at most 100 references.';
      return;
    }
    evidence = [...evidence, { kind: evidenceKind, id: evidenceID.trim() }];
    evidenceID = '';
    evidenceError = '';
  }

  async function save(event: SubmitEvent) {
    event.preventDefault();
    const form = new FormData(event.currentTarget as HTMLFormElement);
    const value = (name: string) => String(form.get(name) ?? '').trim();
    const from = value('from');
    const to = value('to');
    const runID = value('run_id');
    const minDuration = value('min_duration_ms');
    const maxDuration = value('max_duration_ms');
    const errorFilter = value('error');
    const tool = value('tool');
    const input: InvestigationWrite = {
      project_id: value('project_id'),
      filters: {
        ...(from ? { from: new Date(from).toISOString() } : {}),
        ...(to ? { to: new Date(to).toISOString() } : {}),
        ...(runID ? { run_id: runID } : {}),
        ...(minDuration ? { min_duration_ms: Number(minDuration) } : {}),
        ...(maxDuration ? { max_duration_ms: Number(maxDuration) } : {}),
        ...(errorFilter ? { error: errorFilter === 'true' } : {}),
        ...(tool ? { tool } : {})
      },
      evidence,
      summary: value('summary')
    };
    saving = true;
    error = '';
    try {
      const saved = await createInvestigation(input, requestID);
      await goto(resolve(`/investigations/${saved.id}`));
    } catch (cause) {
      error = errorMessage(cause);
    } finally {
      saving = false;
    }
  }
</script>

<svelte:head><title>New investigation · OpenLLMProxy</title></svelte:head>
<a class="text-button" href={resolve('/investigations')}>All investigations</a>
<header>
  <p class="eyebrow">Gateway</p>
  <h1>New investigation</h1>
  <p>
    Save filters, references and your summary. Source payloads are not saved.
  </p>
</header>
{#if projects.error}<p role="alert">{errorMessage(projects.error)}</p>{/if}
{#if error}<p role="alert">{error}</p>{/if}
<form onsubmit={save}>
  <label class="form-field"
    >Project
    <select name="project_id" required value={initialProject}>
      <option value="">Choose a project</option>
      {#each !projects.isFetching && !projects.error ? (projects.data ?? []) : [] as project (project.id)}
        <option value={project.id}>{project.name}</option>
      {/each}
    </select>
  </label>
  <fieldset>
    <legend>Filters</legend>
    <div class="fields">
      <label class="form-field"
        >From <input type="datetime-local" name="from" /></label
      >
      <label class="form-field"
        >To <input type="datetime-local" name="to" /></label
      >
      <label class="form-field"
        >Run ID <input name="run_id" value={initialRun} /></label
      >
      <label class="form-field"
        >Minimum duration (ms) <input
          type="number"
          name="min_duration_ms"
          min="0"
          step="1"
        /></label
      >
      <label class="form-field"
        >Maximum duration (ms) <input
          type="number"
          name="max_duration_ms"
          min="0"
          step="1"
        /></label
      >
      <label class="form-field"
        >Error
        <select name="error"
          ><option value="">Any</option><option value="true">Error</option
          ><option value="false">No error</option></select
        >
      </label>
      <label class="form-field"
        >Tool <input name="tool" maxlength="256" /></label
      >
    </div>
  </fieldset>
  <fieldset>
    <legend>Evidence references ({evidence.length} of 100)</legend>
    <p>
      References must belong to the chosen project. Source evidence may expire
      independently.
    </p>
    {#if evidence.length}
      <ul>
        {#each evidence as item, index (index)}
          <li>
            {item.kind} <code>{item.id}</code>
            <button
              class="button button-secondary"
              type="button"
              aria-label={`Remove ${item.kind} ${item.id}`}
              onclick={() =>
                (evidence = evidence.filter((_, i) => i !== index))}
              >Remove</button
            >
          </li>
        {/each}
      </ul>
    {/if}
    <div class="fields">
      <label class="form-field"
        >Evidence kind
        <select bind:value={evidenceKind}>
          <option value="run">Run</option><option value="step">Step</option
          ><option value="approval">Approval</option><option value="span"
            >Span</option
          ><option value="task">Task</option><option value="delegation"
            >Delegation</option
          >
        </select>
      </label>
      <label class="form-field"
        >Evidence ID <input bind:value={evidenceID} /></label
      >
      <button
        class="button button-secondary"
        type="button"
        onclick={addEvidence}>Add evidence</button
      >
    </div>
    {#if evidenceError}<p role="alert">{evidenceError}</p>{/if}
  </fieldset>
  <label class="form-field"
    >Summary <textarea name="summary" required maxlength="4096" rows="5"
    ></textarea></label
  >
  <button
    class="button button-primary"
    type="submit"
    disabled={saving || projects.isFetching}
    >{saving ? 'Saving…' : 'Save investigation'}</button
  >
</form>

<style>
  header {
    margin: 1rem 0;
  }
  form,
  label {
    display: grid;
    gap: 0.5rem;
  }
  form {
    gap: 1rem;
    max-width: 60rem;
  }
  fieldset {
    padding: 1rem;
  }
  .fields {
    display: flex;
    flex-wrap: wrap;
    align-items: end;
    gap: 1rem;
  }
  input,
  select,
  textarea {
    max-width: 100%;
  }
  code {
    overflow-wrap: anywhere;
  }
  li {
    margin: 0.5rem 0;
  }
</style>
