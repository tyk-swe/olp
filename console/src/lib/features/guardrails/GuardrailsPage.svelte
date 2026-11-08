<script lang="ts">
  import Callout from '#lib/components/Callout.svelte';
  import Dialog from '#lib/components/Dialog.svelte';
  import EmptyState from '#lib/components/EmptyState.svelte';
  import ScrollTable from '#lib/components/ScrollTable.svelte';
  import StatusBadge from '#lib/components/StatusBadge.svelte';
  import { notify } from '#lib/components/toasts.svelte.js';
  import ConfirmAction from '#lib/components/ConfirmAction.svelte';
  import CredentialsPanel from './CredentialsPanel.svelte';
  import AttachmentControls from './AttachmentControls.svelte';
  import { nativeObject, parseManagementJSON } from '#lib/nativeJson.js';
  import {
    useMutationIntent,
    usePrincipalLifetime
  } from '#lib/editing/mutationIntent.svelte.js';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage } from '#lib/management/http.js';
  import { useRole } from '#lib/features/access/session/useRole.svelte.js';
  import {
    listCredentials,
    retireDefinition,
    retirePolicy,
    listDefinitions,
    listPolicies,
    listAttachments,
    listProjects,
    saveDefinition as saveDefinitionAPI,
    savePolicy as savePolicyAPI,
    activateDefinition,
    activatePolicy,
    attachPolicy as attachPolicyAPI,
    evaluatePolicy as evaluatePolicyAPI,
    type Definition,
    type Policy,
    type Entry,
    type Result
  } from './api';
  import { guardrailKeys } from './guardrailKeys';
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
  const phases = [
    'input',
    'input_parallel',
    'output',
    'output_stream'
  ] as const satisfies readonly Entry['phase'][];
  const access = useRole();
  const client = useQueryClient();
  const definitions = createQuery(() => ({
    queryKey: guardrailKeys.definitions(),
    queryFn: listDefinitions
  }));
  const policies = createQuery(() => ({
    queryKey: guardrailKeys.policies(),
    notifyOnChangeProps: 'all',
    queryFn: listPolicies
  }));
  const attachments = createQuery(() => ({
    queryKey: guardrailKeys.attachments(),
    notifyOnChangeProps: 'all',
    queryFn: listAttachments
  }));
  const projects = createQuery(() => ({
    queryKey: guardrailKeys.projects(),
    notifyOnChangeProps: 'all',
    queryFn: listProjects
  }));
  const credentials = createQuery(() => ({
    queryKey: guardrailKeys.credentials(),
    queryFn: listCredentials
  }));
  let credentialID = $state('');
  let creatingCredential = $state(false);
  const definitionIntent = useMutationIntent();
  const capture = usePrincipalLifetime(() => {
    edit(null);
    editPolicy(null);
    configuration = '{}';
    text = '';
    result = null;
  });
  let {
    tab
  }: { tab: 'checks' | 'policies' | 'attachments' | 'credentials' | 'try' } =
    $props();
  let busy = $state(false);
  let error = $state('');
  let definitionOpen = $state(false);
  let policyOpen = $state(false);
  let definitionEditor = 0;
  let selected = $state<Definition | null>(null);
  let name = $state('');
  let project = $state('');
  let type = $state<Definition['type']>('builtin.keywords');
  let configuration = $state('{"words":["restricted"]}');
  let timeout = $state(1000);
  let onError = $state<'fail_closed' | 'fail_open'>('fail_closed');
  let egress = $state(false);
  let policyEditor = 0;
  let selectedPolicy = $state<Policy | null>(null);
  let policyName = $state('');
  let entries = $state<Entry[]>([]);
  let attachPolicy = $state('');
  let scope = $state<
    | 'installation'
    | 'organization'
    | 'project'
    | 'route'
    | 'toolset'
    | 'api-key'
  >('project');
  let scopeID = $state('');
  let order = $state(0);
  let evaluatePolicy = $state('');
  let phase = $state<Entry['phase']>('input');
  // Content stays in component memory and never enters a query cache or storage.
  let text = $state('');
  let result = $state<Result | null>(null);
  const external = $derived(
    !type.startsWith('builtin.') &&
      ![
        'model',
        'semantic_guard',
        'decision_model',
        'typesafe',
        'plugin'
      ].includes(type)
  );
  const canEdit = $derived(access.allows('POST /api/v1/guardrails'));
  async function run(action: (current: () => boolean) => Promise<void>) {
    const current = capture();
    busy = true;
    error = '';
    try {
      await action(current);
      if (current())
        await client.invalidateQueries({ queryKey: guardrailKeys.root });
    } catch (cause) {
      if (current()) error = errorMessage(cause);
    } finally {
      if (current()) busy = false;
    }
  }
  function openDefinition(value: Definition | null) {
    error = '';
    edit(value);
    definitionOpen = true;
  }
  function closeDefinition() {
    edit(null);
    definitionOpen = false;
  }
  function openPolicy(value: Policy | null) {
    error = '';
    editPolicy(value);
    policyOpen = true;
  }
  function closePolicy() {
    editPolicy(null);
    policyOpen = false;
  }
  function edit(value: Definition | null) {
    definitionEditor += 1;
    selected = value;
    name = value?.name ?? '';
    project = value?.project_id ?? project;
    type = value?.type ?? 'builtin.keywords';
    configuration = JSON.stringify(
      value?.configuration ?? { words: ['restricted'] },
      null,
      2
    );
    timeout = value?.timeout_ms ?? 1000;
    onError = value?.on_error ?? 'fail_closed';
    egress = value?.configuration?.content_egress === true;
    credentialID =
      typeof value?.configuration?.credential_ref === 'string'
        ? value.configuration.credential_ref
        : '';
    creatingCredential = false;
  }
  async function saveDefinition(event: SubmitEvent) {
    event.preventDefault();
    const editor = definitionEditor;
    await run(async (current) => {
      const parsed = parseManagementJSON(configuration);
      if (!nativeObject(parsed)) {
        error = 'Configuration must be a JSON object';
        return;
      }
      const config = {
        ...parsed,
        ...(external
          ? {
              content_egress: egress,
              ...(credentialID ? { credential_ref: credentialID } : {})
            }
          : {})
      };
      const saved = await definitionIntent.submit(
        {
          selected,
          project,
          type,
          body: {
            name,
            configuration: config,
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
      );
      if (!current()) return;
      if (editor !== definitionEditor) return;
      selected = saved;
      notify('Draft saved. Activate it when it is ready');
    });
  }
  function editPolicy(value: Policy | null) {
    policyEditor += 1;
    selectedPolicy = value;
    policyName = value?.name ?? '';
    project = value?.project_id ?? project;
    entries = value?.entries.map((entry) => ({ ...entry })) ?? [];
  }
  async function savePolicy(event: SubmitEvent) {
    event.preventDefault();
    const editor = policyEditor;
    await run(async (current) => {
      const saved = await savePolicyAPI(selectedPolicy, project, {
        name: policyName,
        entries
      });
      if (!current()) return;
      if (editor !== policyEditor) return;
      selectedPolicy = saved;
      notify('Policy draft saved');
    });
  }
  function chooseDefinition(index: number, id: string) {
    const definition = definitions.data?.items.find((value) => value.id === id);
    if (definition)
      entries[index] = {
        ...entries[index],
        guardrail_id: id,
        revision_id: definition.revision_id
      };
  }
  async function attach(event: SubmitEvent) {
    event.preventDefault();
    await run(async (current) => {
      await attachPolicyAPI({
        scope,
        scope_id: scope === 'installation' ? 'installation' : scopeID,
        policy_revision_id: attachPolicy,
        order
      });
      if (!current()) return;
      notify('Policy attached');
    });
  }
  async function evaluate(event: SubmitEvent) {
    event.preventDefault();
    const current = capture();
    busy = true;
    error = '';
    result = null;
    try {
      const evaluated = await evaluatePolicyAPI({
        policy_revision_id: evaluatePolicy,
        phase,
        segments: [{ id: 'text', role: 'user', text }]
      });
      if (current()) result = evaluated;
    } catch (cause) {
      if (current()) error = errorMessage(cause);
    } finally {
      if (current()) busy = false;
    }
  }
</script>

<svelte:head><title>Guardrails · OpenLLMProxy</title></svelte:head>
{#if error && !definitionOpen && !policyOpen}<Callout tone="danger"
    >{error}</Callout
  >{/if}
{#if definitions.isPending || policies.isPending || projects.isPending}
  <p role="status">Loading guardrail inventory…</p>
{/if}
{#if definitions.isError || policies.isError || attachments.isError || projects.isError}<Callout
    tone="danger">Guardrails didn’t load. Reload the page to try again.</Callout
  >{/if}

{#if tab === 'checks'}
  <div class="toolbar">
    <p class="lede">
      A check inspects prompts or responses, for example for blocked words or
      personal data. Group checks into a policy to use them.
    </p>
    {#if canEdit}<div class="page-actions">
        <button
          class="button button-primary"
          onclick={() => openDefinition(null)}>New check</button
        >
      </div>{/if}
  </div>
  {#if !definitions.isPending && !definitions.data?.items.length}<EmptyState
      title="No checks yet">Create a check, then add it to a policy.</EmptyState
    >{:else}
    <ScrollTable label="Checks">
      <table class="data-table">
        <thead
          ><tr
            ><th scope="col">Name</th><th scope="col">Type</th><th scope="col"
              >Revision</th
            ><th scope="col"><span class="sr-only">Actions</span></th></tr
          ></thead
        >
        <tbody>
          {#each definitions.data?.items ?? [] as value (value.id)}<tr>
              <td
                ><button
                  class="text-button"
                  onclick={() => openDefinition(value)}>{value.name}</button
                ></td
              >
              <td class="mono">{value.type}</td>
              <td
                >{value.revision}
                <StatusBadge
                  label={value.active_revision_id === value.revision_id
                    ? 'Active'
                    : 'Draft'}
                  tone={value.active_revision_id === value.revision_id
                    ? 'success'
                    : 'warning'}
                /></td
              >
              <td
                >{#if !value.retired_at && access.allows('DELETE /api/v1/guardrails/{guardrail_id}')}<ConfirmAction
                    danger
                    label="Retire check"
                    description={`Retire ${value.name}`}
                    identity={`${value.id}:${value.etag}`}
                    run={(requestId) => retireDefinition(value, requestId)}
                    done={async () => {
                      if (selected?.id === value.id) closeDefinition();
                      await definitions.refetch();
                    }}
                  />{/if}</td
              >
            </tr>{/each}
        </tbody>
      </table>
    </ScrollTable>
  {/if}
  {#if definitionOpen}
    <Dialog
      title={selected ? selected.name : 'New check'}
      description={selected
        ? `Revision ${selected.revision}. Saving creates a new draft revision.`
        : undefined}
      placement="side"
      size="wide"
      onClose={closeDefinition}
    >
      {#if error}<Callout tone="danger">{error}</Callout>{/if}
      <form onsubmit={saveDefinition}>
        <label class="form-field"
          >Name<input required maxlength="100" bind:value={name} /></label
        >
        <label class="form-field"
          >Project<select required bind:value={project} disabled={!!selected}
            ><option value="">Choose a project</option
            >{#each projects.data?.items ?? [] as value (value.id)}<option
                value={value.id}>{value.name}</option
              >{/each}</select
          ></label
        >
        <label class="form-field"
          >Type<select bind:value={type} disabled={!!selected}
            >{#each guardrailTypes as value (value)}<option {value}
                >{value}</option
              >{/each}</select
          ></label
        >
        <label class="form-field"
          >Configuration (JSON)<textarea
            required
            rows="8"
            spellcheck="false"
            bind:value={configuration}></textarea></label
        >
        {#if external}
          <label class="form-field"
            >Credential<select bind:value={credentialID}
              ><option value="">Use configuration reference</option
              >{#each (credentials.data ?? []).filter((item) => item.project_id === project) as item (item.id)}<option
                  value={item.id}>{item.name}</option
                >{/each}</select
            ></label
          >
          <button
            class="button button-secondary"
            type="button"
            disabled={!project}
            onclick={() => {
              creatingCredential = !creatingCredential;
            }}>Create replacement credential</button
          >
          <label class="check-field"
            ><input type="checkbox" required bind:checked={egress} />I
            acknowledge that this guardrail sends inspected content to its
            configured endpoint.</label
          >{/if}
        <label class="form-field"
          >Timeout (milliseconds)<input
            type="number"
            min="1"
            max="300000"
            required
            bind:value={timeout}
          /></label
        >
        <label class="form-field"
          >On evaluation error<select bind:value={onError}
            ><option value="fail_closed">Block the request</option><option
              value="fail_open">Continue and record the failure</option
            ></select
          ></label
        >
        <button class="button button-primary" disabled={busy} type="submit"
          >Save draft</button
        >
        {#if selected}<button
            class="button button-secondary"
            disabled={busy}
            type="button"
            onclick={() =>
              run(async (current) => {
                if (!selected) return;
                await activateDefinition(selected);
                if (!current()) return;
                notify('Check activated');
                closeDefinition();
              })}>Activate revision {selected.revision}</button
          >{/if}
      </form>
      {#if creatingCredential}<CredentialsPanel
          projects={projects.data?.items ?? []}
          projectID={project}
          oncreated={(id) => {
            credentialID = id;
            creatingCredential = false;
          }}
        />{/if}
    </Dialog>
  {/if}
{:else if tab === 'policies'}
  <div class="toolbar">
    <p class="lede">
      A policy runs its checks in order. Attach a policy to apply it to an
      installation, project, route or key.
    </p>
    {#if canEdit}<div class="page-actions">
        <button class="button button-primary" onclick={() => openPolicy(null)}
          >New policy</button
        >
      </div>{/if}
  </div>
  {#if !policies.isPending && !policies.data?.items.length}<EmptyState
      title="No policies yet"
      >Create a policy from one or more checks.</EmptyState
    >{:else}
    <ScrollTable label="Policies">
      <table class="data-table">
        <thead
          ><tr
            ><th scope="col">Name</th><th scope="col">Revision</th><th
              scope="col"><span class="sr-only">Actions</span></th
            ></tr
          ></thead
        >
        <tbody>
          {#each policies.data?.items ?? [] as value (value.policy_id)}<tr>
              <td
                ><button class="text-button" onclick={() => openPolicy(value)}
                  >{value.name}</button
                ></td
              >
              <td>{value.revision}</td>
              <td
                >{#if !value.retired_at && access.allows('DELETE /api/v1/guardrail-policies/{policy_id}')}<ConfirmAction
                    danger
                    label="Retire policy"
                    description={`Retire ${value.name}`}
                    identity={`${value.policy_id}:${value.etag}`}
                    run={(requestId) => retirePolicy(value, requestId)}
                    done={async () => {
                      if (selectedPolicy?.policy_id === value.policy_id)
                        closePolicy();
                      await policies.refetch();
                    }}
                  />{/if}</td
              >
            </tr>{/each}
        </tbody>
      </table>
    </ScrollTable>
  {/if}
  {#if policyOpen}
    <Dialog
      title={selectedPolicy ? selectedPolicy.name : 'New policy'}
      placement="side"
      size="wide"
      onClose={closePolicy}
    >
      {#if error}<Callout tone="danger">{error}</Callout>{/if}
      <form onsubmit={savePolicy}>
        <label class="form-field"
          >Name<input required maxlength="100" bind:value={policyName} /></label
        >
        <label class="form-field"
          >Project<select
            required
            bind:value={project}
            disabled={!!selectedPolicy}
            ><option value="">Choose a project</option
            >{#each projects.data?.items ?? [] as value (value.id)}<option
                value={value.id}>{value.name}</option
              >{/each}</select
          ></label
        >
        <ol>
          {#each entries as entry, i (i)}<li class="entry">
              <label class="form-field"
                >Guardrail<select
                  required
                  value={entry.guardrail_id}
                  onchange={(event) =>
                    chooseDefinition(i, event.currentTarget.value)}
                  ><option value="">Choose a definition</option
                  >{#each (definitions.data?.items ?? []).filter((value) => value.project_id === project) as value (value.id)}<option
                      value={value.id}
                      >{value.name} · Revision {value.revision}</option
                    >{/each}</select
                ></label
              >
              <label class="form-field"
                >Phase<select bind:value={entry.phase}
                  >{#each phases as value (value)}<option {value}
                      >{value}</option
                    >{/each}</select
                ></label
              >
              <label class="form-field"
                >Action<select bind:value={entry.action}
                  >{#each ['block', 'redact', 'mask', 'annotate'] as value (value)}<option
                      {value}>{value}</option
                    >{/each}</select
                ></label
              >
              <label class="check-field"
                ><input type="checkbox" bind:checked={entry.monitor} />Monitor
                only</label
              ><label class="check-field"
                ><input
                  type="checkbox"
                  bind:checked={entry.mandatory}
                />Mandatory</label
              >
              <button
                class="button button-secondary"
                type="button"
                onclick={() => {
                  entries = entries.filter((_, index) => index !== i);
                }}>Remove entry {i + 1}</button
              >
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
          }}>Add guardrail</button
        >
        <button class="button button-primary" type="submit" disabled={busy}
          >Save policy draft</button
        >
        {#if selectedPolicy}<button
            class="button button-secondary"
            type="button"
            disabled={busy}
            onclick={() =>
              run(async (current) => {
                if (!selectedPolicy) return;
                await activatePolicy(selectedPolicy);
                if (!current()) return;
                notify(
                  'Policy activated. Existing attachments keep their pinned revision'
                );
                closePolicy();
              })}>Activate revision</button
          >{/if}
      </form>
    </Dialog>
  {/if}
{:else if tab === 'attachments'}
  <section class="card stack-card">
    <h2>Inherited safeguards</h2>
    <p>
      Policies run in installation, organization, project, route or toolset, and
      key order. Each attachment adds safeguards, and narrower scopes cannot
      remove an inherited policy
    </p>
    <ul>
      {#each attachments.data?.items ?? [] as value (value.id)}<li>
          {value.scope} · {value.scope_id} · Position {value.order} · Policy revision
          {value.policy_revision_id}
          {#if value.id && value.etag}{#key value.policy_revision_id}<AttachmentControls
                value={{ ...value, id: value.id, etag: value.etag }}
                policies={policies.data?.items ?? []}
                changed={async () => {
                  await client.invalidateQueries({
                    queryKey: guardrailKeys.root
                  });
                }}
              />{/key}{/if}
        </li>{/each}
    </ul>
    {#if canEdit}<form onsubmit={attach}>
        <label class="form-field"
          >Policy revision<select required bind:value={attachPolicy}
            ><option value="">Choose a policy</option
            >{#each policies.data?.items ?? [] as value (value.policy_id)}<option
                value={value.revision_id}
                >{value.name} · Revision {value.revision}</option
              >{/each}</select
          ></label
        >
        <label class="form-field"
          >Scope<select bind:value={scope}
            >{#each ['installation', 'organization', 'project', 'route', 'toolset', 'api-key'] as value (value)}<option
                {value}>{value}</option
              >{/each}</select
          ></label
        >
        {#if scope !== 'installation'}<label class="form-field"
            >Scope identifier<input required bind:value={scopeID} /></label
          >{/if}
        <label class="form-field"
          >Position<input
            type="number"
            min="0"
            required
            bind:value={order}
          /></label
        ><button class="button button-primary" disabled={busy}
          >Attach policy</button
        >
      </form>{/if}
  </section>
{:else if tab === 'credentials'}
  <CredentialsPanel projects={projects.data?.items ?? []} />
{:else}
  <section class="card stack-card">
    <h2>Try a policy</h2>
    <p>
      Test a policy without saving the content you enter. External guardrails
      send it to their configured endpoints.
    </p>
    <form onsubmit={evaluate}>
      <label class="form-field"
        >Policy revision<select required bind:value={evaluatePolicy}
          ><option value="">Choose a policy</option
          >{#each policies.data?.items ?? [] as value (value.policy_id)}<option
              value={value.revision_id}
              >{value.name} · Revision {value.revision}</option
            >{/each}</select
        ></label
      >
      <label class="form-field"
        >Phase<select bind:value={phase}
          >{#each phases as value (value)}<option {value}>{value}</option
            >{/each}</select
        ></label
      >
      <label class="form-field"
        >Text<textarea rows="8" required bind:value={text}></textarea></label
      ><button
        class="button button-secondary"
        disabled={busy || !access.allows('POST /api/v1/guardrails/evaluate')}
        >Evaluate</button
      ><button
        class="button button-secondary"
        type="button"
        onclick={() => {
          text = '';
          result = null;
        }}>Clear content</button
      >
    </form>
    {#if result}<div role="status">
        <h3>{result.blocked ? 'Blocked' : 'Allowed'}</h3>
        <ul>
          {#each result.decisions as decision (`${decision.guardrail_id}-${decision.phase}-${decision.outcome}`)}<li
            >
              {decision.guardrail_id} · {decision.phase} · {decision.outcome} · {(
                decision.categories ?? []
              ).join(', ')}
            </li>{/each}
        </ul>
        <pre class="code-block">{result.segments
            .map((segment) => segment.text)
            .join('\n')}</pre>
      </div>{/if}
  </section>
{/if}

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
