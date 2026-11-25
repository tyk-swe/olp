<script lang="ts">
  import Callout from '#lib/components/Callout.svelte';
  import Dialog from '#lib/components/Dialog.svelte';
  import CredentialsPanel from './CredentialsPanel.svelte';
  import { notify } from '#lib/components/toasts.svelte.js';
  import { errorMessage } from '#lib/management/http.js';
  import { nativeObject, parseManagementJSON } from '#lib/nativeJson.js';
  import type { useAction } from '#lib/editing/action.svelte.js';
  import type { useMutationIntent } from '#lib/editing/mutationIntent.svelte.js';
  import {
    activateDefinition,
    saveDefinition as saveDefinitionAPI,
    type Credential,
    type Definition
  } from './api';
  import type { Project } from '#lib/features/access/index.js';

  let {
    action,
    refresh,
    definitionIntent,
    definitionActivationIntent,
    definitionEditor,
    selected = $bindable(),
    busy,
    error = $bindable(),
    external,
    projects,
    credentials,
    name = $bindable(),
    project = $bindable(),
    type = $bindable(),
    configuration = $bindable(),
    credentialID = $bindable(),
    timeout = $bindable(),
    egress = $bindable(),
    onError = $bindable(),
    creatingCredential = $bindable(),
    onClose
  }: {
    action: ReturnType<typeof useAction>;
    refresh: (current: () => boolean) => Promise<void>;
    definitionIntent: ReturnType<typeof useMutationIntent>;
    definitionActivationIntent: ReturnType<typeof useMutationIntent>;
    definitionEditor: number;
    selected: Definition | null;
    busy: boolean;
    error: string;
    external: boolean;
    projects: Project[];
    credentials: Credential[];
    name: string;
    project: string;
    type: Definition['type'];
    configuration: string;
    credentialID: string;
    timeout: number;
    egress: boolean;
    onError: 'fail_closed' | 'fail_open';
    creatingCredential: boolean;
    onClose: () => void;
  } = $props();

  const guardrailTypes = [
    'builtin.regex',
    'builtin.pii',
    'builtin.secrets',
    'builtin.keywords',
    'builtin.schema',
    'builtin.tools',
    'builtin.code_execution',
    'builtin.tool_permission',
    'webhook',
    'bedrock',
    'azure.content_safety',
    'azure.prompt_shields',
    'model_armor',
    'lakera',
    'presidio',
    'model',
    'plugin',
    'aim',
    'akto',
    'alice',
    'aporia',
    'cato_networks',
    'cisco_ai_defense',
    'compresr',
    'conduct',
    'crowdstrike_aidr',
    'deepkeep',
    'dynamoai',
    'enkryptai',
    'generic_guardrail_api',
    'grayswan',
    'guardrails_ai',
    'headroom',
    'hiddenlayer',
    'ibm_guardrails',
    'javelin',
    'lakera_v1',
    'lasso',
    'microsoft_purview',
    'noma',
    'noma_v2',
    'onyx',
    'openai_moderation',
    'ovalix',
    'pangea',
    'panw_prisma_airs',
    'pillar',
    'prompt_security',
    'promptguard',
    'qostodian_nexus',
    'qualifire',
    'repelloai',
    'rubrik',
    'singulr',
    'straiker',
    'vigil_guard',
    'xecguard',
    'zscaler_ai_guard',
    'semantic_guard',
    'decision_model',
    'typesafe'
  ] as const satisfies readonly Definition['type'][];

  async function saveDefinition(event: SubmitEvent) {
    event.preventDefault();
    const parsed = parseManagementJSON(configuration);
    if (!nativeObject(parsed)) {
      error = 'Configuration must be a JSON object';
      return;
    }
    const editor = definitionEditor;
    error = '';
    await action.run(
      () =>
        definitionIntent.submit(
          {
            selected,
            project,
            type,
            body: {
              name,
              configuration: {
                ...parsed,
                ...(external
                  ? {
                      content_egress: egress,
                      ...(credentialID ? { credential_ref: credentialID } : {})
                    }
                  : {})
              },
              timeout_ms: timeout,
              on_error: onError
            }
          },
          (input, requestId) =>
            saveDefinitionAPI(
              input.selected,
              input.project,
              input.type,
              input.body,
              requestId
            )
        ),
      async (saved, current) => {
        if (!current()) return;
        if (editor === definitionEditor) {
          selected = saved;
          notify('Draft saved. Activate it when it is ready');
        }
        await refresh(current);
      },
      { key: 'definition', failure: (cause) => errorMessage(cause) }
    );
    error = action.error;
  }

  async function activateDefinitionRevision() {
    const definition = selected;
    if (!definition) return;
    error = '';
    await action.run(
      () =>
        definitionActivationIntent.submit(definition, (input, requestId) =>
          activateDefinition(input, requestId)
        ),
      async (_, current) => {
        if (!current()) return;
        notify('Check activated');
        onClose();
        await refresh(current);
      },
      { key: 'activate', failure: (cause) => errorMessage(cause) }
    );
    error = action.error;
  }
</script>

<Dialog
  title={selected ? selected.name : 'New check'}
  description={selected
    ? `Revision ${selected.revision}. Saving creates a new draft revision.`
    : undefined}
  placement="side"
  size="wide"
  {onClose}
>
  {#if error}<Callout tone="danger">{error}</Callout>{/if}
  <form onsubmit={saveDefinition}>
    <label class="form-field">
      Name<input required maxlength="100" bind:value={name} />
    </label>
    <label class="form-field">
      Project<select required bind:value={project} disabled={!!selected}>
        <option value="">
          Choose a project
        </option>{#each projects as value (value.id)}<option value={value.id}>
            {value.name}
          </option>{/each}
      </select>
    </label>
    <label class="form-field">
      Type<select bind:value={type} disabled={!!selected}>
        {#each guardrailTypes as value (value)}<option {value}>
            {value}
          </option>{/each}
      </select>
    </label>
    <label class="form-field">
      Configuration (JSON)<textarea
        required
        rows="8"
        spellcheck="false"
        bind:value={configuration}></textarea>
    </label>
    {#if external}
      <label class="form-field">
        Credential<select bind:value={credentialID}>
          <option value="">
            Use configuration reference
          </option>{#each credentials.filter((item) => item.project_id === project) as item (item.id)}<option
              value={item.id}
            >
              {item.name}
            </option>{/each}
        </select>
      </label>
      <button
        class="button button-secondary"
        type="button"
        disabled={!project}
        onclick={() => {
          creatingCredential = !creatingCredential;
        }}
      >
        Create replacement credential
      </button>
      <label class="check-field">
        <input type="checkbox" required bind:checked={egress} />I acknowledge
        that this guardrail sends inspected content to its configured endpoint.
      </label>{/if}
    <label class="form-field">
      Timeout (milliseconds)<input
        type="number"
        min="1"
        max="300000"
        required
        bind:value={timeout}
      />
    </label>
    <label class="form-field">
      On evaluation error<select bind:value={onError}>
        <option value="fail_closed">Block the request</option><option
          value="fail_open"
        >
          Continue and record the failure
        </option>
      </select>
    </label>
    <button class="button button-primary" disabled={busy} type="submit">
      Save draft
    </button>
    {#if selected}<button
        class="button button-secondary"
        disabled={busy}
        type="button"
        onclick={activateDefinitionRevision}
      >
        Activate revision {selected.revision}
      </button>{/if}
  </form>
  {#if creatingCredential}<CredentialsPanel
      {projects}
      projectID={project}
      oncreated={(id) => {
        credentialID = id;
        creatingCredential = false;
      }}
    />{/if}
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
</style>
