<script lang="ts">
  import PublicationEditor from './PublicationEditor.svelte';
  import PublicationList from './PublicationList.svelte';
  import { useAction } from '#lib/editing/action.svelte.js';
  import { readResult } from '#lib/editing/result.js';
  import Callout from '#lib/components/Callout.svelte';
  import { guardUnsavedChanges } from '#lib/editing/unsavedChanges.js';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { errorMessage } from '#lib/management/http.js';
  import { projectQueries } from '#lib/domain/projects/queries.js';
  import { useRole } from '#lib/session/useRole.svelte.js';
  import {
    useMutationIntent,
    usePrincipalLifetime
  } from '#lib/editing/mutationIntent.svelte.js';

  import {
    getPublication,
    getRevision,
    getServer,
    listPublications,
    listServers,
    executePublication,
    saveServer,
    type PublicationReceipt,
    type Kind,
    type MCPServer,
    type Publication,
    type Revision
  } from './api';
  import { agentKeys } from './agentKeys';
  import {
    publicationCommand,
    publicationForm,
    publicationInput,
    serverPricingForm,
    serverPricingInput,
    toolPin,
    type PublicationAction,
    type ServerPricingForm
  } from './form';

  const access = useRole();
  const client = useQueryClient();
  const intent = useMutationIntent();
  let { kind }: { kind: Kind } = $props();

  const EMPTY: Record<Kind, { title: string; body: string }> = {
    toolsets: {
      title: 'No toolsets yet',
      body: 'A toolset bundles MCP tools so routes and agents can use them together.'
    },
    agents: {
      title: 'No agents yet',
      body: 'Publish an agent so other agents and API keys can delegate work to it.'
    },
    prompts: {
      title: 'No prompts yet',
      body: 'Publish a prompt so it can be reused by name.'
    }
  };
  let current = $state<Publication | null>(null);
  let form = $state(publicationForm());
  let editor = $state(false);
  let readBusy = $state(false);
  const pubAction = useAction();
  const busy = $derived(readBusy || pubAction.busy);
  let error = $state('');
  let notice = $state('');
  let pendingReceipt = $state<PublicationReceipt | null>(null);
  let credential = $state('');
  let entraClientSecret = $state('');
  let removeCredential = $state(false);
  let serverID = $state('');
  let server = $state<MCPServer | null>(null);
  let pricing = $state<ServerPricingForm | null>(null);
  let revisionID = $state('');
  let inspected = $state<Revision | null>(null);
  let confirmRetire = $state(false);
  let epoch = 0;
  const captureOwner = usePrincipalLifetime(() => {
    epoch += 1;
    credential = '';
    entraClientSecret = '';
    removeCredential = false;
    current = null;
    form = publicationForm();
    inspected = null;
    server = null;
    pricing = null;
    editor = false;
    error = '';
    notice = '';
    pendingReceipt = null;
  });
  const items = createQuery(() => ({
    queryKey: agentKeys.publication(kind),
    queryFn: () => listPublications(kind)
  }));
  const projects = createQuery(projectQueries.all);
  const servers = createQuery(() => ({
    queryKey: agentKeys.servers(),
    queryFn: listServers,
    enabled: kind === 'toolsets'
  }));
  const canManage = $derived(access.allows('POST /api/v1/toolsets'));
  const canPrice = $derived(
    access.allows('PUT /api/v1/mcp-servers/{server_id}')
  );
  const canTrust = $derived(
    access.user?.operations.includes('access') && access.globalScope
  );
  const unsaved = $derived(
    current !== null &&
      JSON.stringify(publicationInput(kind, form)) !==
        JSON.stringify(publicationInput(kind, publicationForm(current)))
  );

  guardUnsavedChanges(() => unsaved);

  function abandon() {
    epoch += 1;
    intent.reset();
    credential = '';
    entraClientSecret = '';
    removeCredential = false;
    editor = false;
    current = null;
    inspected = null;
    server = null;
    pricing = null;
    serverID = '';
    error = '';
    confirmRetire = false;
    pendingReceipt = null;
  }
  async function edit(id?: string) {
    abandon();
    const version = epoch;
    const isCurrent = captureOwner();
    error = '';
    readBusy = true;
    const result = await readResult(() =>
      id ? getPublication(kind, id) : Promise.resolve(null)
    );
    if (!isCurrent() || version !== epoch) return;
    readBusy = false;
    if (!result.ok) {
      error = errorMessage(result.error);
      return;
    }
    const value = result.value;
    current = value;
    form = publicationForm(value);
    if (!value) form.project = projects.data?.[0]?.id ?? '';
    revisionID = value?.revision_id ?? '';
    editor = true;
  }
  async function chooseServer() {
    server = null;
    pricing = null;
    const selected = serverID;
    const version = epoch;
    const isCurrent = captureOwner();
    if (!selected) return;
    const result = await readResult(() => getServer(selected));
    if (!result.ok) {
      if (isCurrent() && version === epoch) error = errorMessage(result.error);
      return;
    }
    if (isCurrent() && version === epoch && selected === serverID) {
      server = result.value;
      pricing = serverPricingForm(result.value);
    }
  }
  async function savePricing() {
    if (!server || !pricing) return;
    const version = epoch;
    const isCurrent = captureOwner();
    const observed = server;
    const draft = pricing;
    error = '';
    notice = '';
    const pricingResult = serverPricingInput(observed, draft);
    if (!pricingResult.ok) {
      error = pricingResult.message;
      return;
    }
    const input = pricingResult.input;
    await pubAction.run(
      () =>
        intent.submit(
          { action: 'server-pricing', observed, input },
          async (submission, requestId) =>
            saveServer(submission.observed, submission.input, requestId)
        ),
      async (result, current) => {
        if (!current() || version !== epoch) return;
        server = result;
        pricing = serverPricingForm(result);
        serverID = result.id;
        notice =
          'Server pricing and limits saved. Republish toolsets to pin new prices, and limits apply to the server now.';
        const refreshed = await readResult(() =>
          client.invalidateQueries(
            { queryKey: agentKeys.servers() },
            { throwOnError: true }
          )
        );
        if (!refreshed.ok && isCurrent() && version === epoch)
          error = `Write saved, but refreshing failed. ${errorMessage(refreshed.error)}`;
      },
      {
        key: 'pricing',
        failure: (cause) => errorMessage(cause)
      }
    );
    if (version === epoch && pubAction.error) error = pubAction.error;
  }
  async function addTool(name: string) {
    if (!server) return;
    const version = epoch;
    const isCurrent = captureOwner();
    const pinned = server;
    const result = await readResult(() => toolPin(pinned, name));
    if (!result.ok) {
      if (isCurrent() && version === epoch) error = errorMessage(result.error);
      return;
    }
    const pinnedResult = result.value;
    if (!pinnedResult.ok) {
      if (isCurrent() && version === epoch) error = pinnedResult.message;
      return;
    }
    const pin = pinnedResult.pin;
    if (
      isCurrent() &&
      version === epoch &&
      !form.tools.some((tool) => tool.alias === pin.alias)
    )
      form.tools = [...form.tools, pin];
  }
  async function inspect() {
    if (!current) return;
    const version = epoch;
    const isCurrent = captureOwner();
    const id = current.id;
    const revision = revisionID;
    const result = await readResult(() => getRevision(kind, id, revision));
    if (!result.ok) {
      if (isCurrent() && version === epoch) error = errorMessage(result.error);
      return;
    }
    if (isCurrent() && version === epoch && revision === revisionID)
      inspected = result.value;
  }
  async function refreshReceipt(
    receipt: PublicationReceipt,
    isCurrent: () => boolean
  ) {
    pendingReceipt = receipt;
    const result = await readResult(() =>
      client.invalidateQueries(
        { queryKey: agentKeys.publications },
        { throwOnError: true }
      )
    );
    if (!isCurrent()) return;
    if (result.ok) pendingReceipt = null;
    else
      error = `Write saved, but refreshing failed. ${errorMessage(result.error)}`;
  }
  async function retryRefresh() {
    const receipt = pendingReceipt;
    if (!receipt || busy) return;
    const version = epoch;
    const owner = captureOwner();
    const isCurrent = () => owner() && version === epoch;
    readBusy = true;
    error = '';
    await refreshReceipt(receipt, isCurrent);
    if (isCurrent()) readBusy = false;
  }
  async function write(action: PublicationAction) {
    const compiled = publicationCommand({
      kind,
      current,
      action,
      form,
      credential,
      entraClientSecret,
      removeCredential,
      revision: revisionID
    });
    if (!compiled.ok) {
      error = compiled.message;
      return;
    }
    const version = epoch;
    const owner = captureOwner();
    const isCurrent = () => owner() && version === epoch;
    error = '';
    notice = '';
    await pubAction.run(
      () => intent.submit(compiled.command, executePublication),
      async (receipt, live) => {
        if (!isCurrent() || !live()) return;
        credential = '';
        entraClientSecret = '';
        removeCredential = false;
        current = receipt.kind === 'updated' ? receipt.publication : null;
        inspected = null;
        confirmRetire = false;
        if (current) {
          form = publicationForm(current);
          revisionID = current.revision_id ?? '';
        } else editor = false;
        notice = receipt.notice;
        await refreshReceipt(receipt, isCurrent);
      },
      { key: 'publication', failure: (cause) => errorMessage(cause) }
    );
    if (isCurrent() && pubAction.error) error = pubAction.error;
  }
</script>

{#if error}<p class="inline-problem" role="alert">
    {error}
  </p>{/if}{#if notice}<Callout tone="success">{notice}</Callout>{/if}
{#if items.error}<p class="inline-problem" role="alert">
    {errorMessage(items.error)}
  </p>{/if}
{#if pendingReceipt}<button
    class="button button-secondary"
    type="button"
    disabled={busy}
    onclick={retryRefresh}
  >
    Retry refresh
  </button>{/if}
<PublicationList
  {kind}
  {busy}
  canCreate={canManage && (kind !== 'agents' || !!canTrust)}
  pending={items.isPending}
  items={items.data ?? []}
  projects={projects.data ?? []}
  empty={EMPTY[kind]}
  onCreate={() => edit()}
  onOpen={edit}
/>

{#if editor}
  <PublicationEditor
    {kind}
    {current}
    {busy}
    {canManage}
    {canPrice}
    {canTrust}
    {unsaved}
    {revisionID}
    {inspected}
    projects={projects.data ?? []}
    servers={servers.data ?? []}
    bind:form
    bind:server
    bind:serverID
    bind:pricing
    bind:credential
    bind:removeCredential
    bind:entraClientSecret
    bind:confirmRetire
    onWrite={write}
    onAbandon={abandon}
    onChooseServer={chooseServer}
    onSavePricing={savePricing}
    onAddTool={addTool}
    onInspect={inspect}
  />
{/if}
