<script lang="ts">
  import Callout from '#lib/components/Callout.svelte';
  import Dialog from '#lib/components/Dialog.svelte';
  import { notify } from '#lib/components/toasts.svelte.js';
  import { errorMessage } from '#lib/management/http.js';
  import type { useAction } from '#lib/editing/action.svelte.js';
  import type { useMutationIntent } from '#lib/editing/mutationIntent.svelte.js';
  import {
    activatePolicy,
    savePolicy as savePolicyAPI,
    type Definition,
    type Entry,
    type Policy
  } from './api';
  import type { Project } from '#lib/domain/projects/api.js';

  let {
    action,
    refresh,
    policyIntent,
    policyActivationIntent,
    policyEditor,
    selectedPolicy = $bindable(),
    busy,
    error = $bindable(),
    phases,
    definitions,
    projects,
    policyName = $bindable(),
    project = $bindable(),
    entries = $bindable(),
    onClose
  }: {
    action: ReturnType<typeof useAction>;
    refresh: (current: () => boolean) => Promise<void>;
    policyIntent: ReturnType<typeof useMutationIntent>;
    policyActivationIntent: ReturnType<typeof useMutationIntent>;
    policyEditor: number;
    selectedPolicy: Policy | null;
    busy: boolean;
    error: string;
    phases: readonly Entry['phase'][];
    definitions: Definition[];
    projects: Project[];
    policyName: string;
    project: string;
    entries: Entry[];
    onClose: () => void;
  } = $props();

  function chooseDefinition(index: number, id: string) {
    const definition = definitions.find((value) => value.id === id);
    if (definition)
      entries[index] = {
        ...entries[index],
        guardrail_id: id,
        revision_id: definition.revision_id
      };
  }

  async function savePolicy(event: SubmitEvent) {
    event.preventDefault();
    const editor = policyEditor;
    error = '';
    await action.run(
      () =>
        policyIntent.submit(
          {
            selected: selectedPolicy,
            project,
            body: { name: policyName, entries }
          },
          (input, requestId) =>
            savePolicyAPI(input.selected, input.project, input.body, requestId)
        ),
      async (saved, current) => {
        if (!current()) return;
        if (editor === policyEditor) {
          selectedPolicy = saved;
          notify('Policy draft saved');
        }
        await refresh(current);
      },
      { key: 'policy', failure: (cause) => errorMessage(cause) }
    );
    error = action.error;
  }

  async function activatePolicyRevision() {
    const policy = selectedPolicy;
    if (!policy) return;
    error = '';
    await action.run(
      () =>
        policyActivationIntent.submit(policy, (input, requestId) =>
          activatePolicy(input, requestId)
        ),
      async (_, current) => {
        if (!current()) return;
        notify(
          'Policy activated. Existing attachments keep their pinned revision'
        );
        onClose();
        await refresh(current);
      },
      { key: 'activate', failure: (cause) => errorMessage(cause) }
    );
    error = action.error;
  }
</script>

<Dialog
  title={selectedPolicy ? selectedPolicy.name : 'New policy'}
  placement="side"
  size="wide"
  {onClose}
>
  {#if error}<Callout tone="danger">{error}</Callout>{/if}
  <form onsubmit={savePolicy}>
    <label class="form-field">
      Name<input required maxlength="100" bind:value={policyName} />
    </label>
    <label class="form-field">
      Project<select required bind:value={project} disabled={!!selectedPolicy}>
        <option value="">
          Choose a project
        </option>{#each projects as value (value.id)}<option value={value.id}>
            {value.name}
          </option>{/each}
      </select>
    </label>
    <ol>
      {#each entries as entry, i (i)}<li class="entry">
          <label class="form-field">
            Guardrail<select
              required
              value={entry.guardrail_id}
              onchange={(event) =>
                chooseDefinition(i, event.currentTarget.value)}
            >
              <option value="">
                Choose a definition
              </option>{#each definitions.filter((value) => value.project_id === project) as value (value.id)}<option
                  value={value.id}
                >
                  {value.name} · Revision {value.revision}
                </option>{/each}
            </select>
          </label>
          <label class="form-field">
            Phase<select bind:value={entry.phase}>
              {#each phases as value (value)}<option {value}>
                  {value}
                </option>{/each}
            </select>
          </label>
          <label class="form-field">
            Action<select bind:value={entry.action}>
              {#each ['block', 'redact', 'mask', 'annotate'] as value (value)}<option
                  {value}
                >
                  {value}
                </option>{/each}
            </select>
          </label>
          <label class="check-field">
            <input type="checkbox" bind:checked={entry.monitor} />Monitor only
          </label><label class="check-field">
            <input type="checkbox" bind:checked={entry.mandatory} />Mandatory
          </label>
          <button
            class="button button-secondary"
            type="button"
            onclick={() => {
              entries = entries.filter((_, index) => index !== i);
            }}
          >
            Remove entry {i + 1}
          </button>
        </li>{/each}
    </ol>
    <button
      class="button button-secondary"
      type="button"
      onclick={() => {
        entries = [
          ...entries,
          {
            guardrail_id: '',
            revision_id: '',
            phase: 'input',
            action: 'block',
            monitor: false,
            mandatory: true
          }
        ];
      }}
    >
      Add guardrail
    </button>
    <button class="button button-primary" type="submit" disabled={busy}>
      Save policy draft
    </button>
    {#if selectedPolicy}<button
        class="button button-secondary"
        type="button"
        disabled={busy}
        onclick={activatePolicyRevision}
      >
        Activate revision
      </button>{/if}
  </form>
</Dialog>

<style>
  form {
    display: grid;
    gap: 1rem;
    max-width: 52rem;
  }
  form > .button {
    justify-self: start;
  }
  .entry {
    margin-block: 1rem;
    padding: 1rem;
    border: 1px solid var(--border-hairline);
    border-radius: var(--radius-control);
  }
</style>
