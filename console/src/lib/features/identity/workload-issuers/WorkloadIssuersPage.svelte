<script lang="ts">
  import PageTitle from '#lib/shell/branding/PageTitle.svelte';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { useRole } from '#lib/session/useRole.svelte.js';
  import {
    useMutationIntent,
    usePrincipalLifetime
  } from '#lib/editing/mutationIntent.svelte.js';
  import { useAction } from '#lib/editing/action.svelte.js';
  import { focusFormError } from '#lib/editing/focusError.js';
  import FormField from '#lib/components/FormField.svelte';
  import QueryView from '#lib/components/QueryView.svelte';
  import {
    listIssuers,
    saveIssuer,
    issuerInput,
    type Issuer,
    type IssuerWrite
  } from './api';
  import { issuerDraft, validateIssuer, type IssuerField } from './issuerDraft';
  import { workloadIssuerKeys } from './workloadIssuerKeys';
  import Callout from '#lib/components/Callout.svelte';
  import Dialog from '#lib/components/Dialog.svelte';
  import EmptyState from '#lib/components/EmptyState.svelte';
  import ScrollTable from '#lib/components/ScrollTable.svelte';
  import StatusBadge from '#lib/components/StatusBadge.svelte';
  import { confirmAction } from '#lib/components/confirm.js';
  import { notify } from '#lib/components/toasts.svelte.js';

  const access = useRole();
  const client = useQueryClient();
  const query = createQuery(() => ({
    queryKey: workloadIssuerKeys.root,
    queryFn: ({ signal }) => listIssuers(signal),
    enabled: access.allows('GET /api/v1/workload-issuers')
  }));
  const editable = $derived(access.allows('POST /api/v1/workload-issuers'));
  const id = $props.id();
  const signatureAlgorithms: IssuerWrite['algorithms'] = [
    'RS256',
    'ES256',
    'EdDSA'
  ];
  let editing = $state<Issuer>();
  let opened = $state(false);
  let draft = $state(issuerDraft());
  let pending = $state<IssuerWrite>();
  const write = useAction<IssuerField>();
  const saveIntent = useMutationIntent();
  const disableIntent = useMutationIntent();
  usePrincipalLifetime(() => {
    editing = pending = undefined;
    opened = false;
    draft = issuerDraft();
  });
  function open(value?: Issuer) {
    editing = value;
    draft = issuerDraft(value);
    pending = undefined;
    write.reset();
    saveIntent.reset();
    opened = true;
  }
  function lock() {
    const checked = validateIssuer(draft);
    if (!write.check(checked)) return;
    pending = checked.value;
    return checked.value;
  }
  async function submit(event: SubmitEvent) {
    event.preventDefault();
    if (write.busy || !editable) return;
    const form = event.currentTarget as HTMLFormElement;
    const submitted = pending ?? lock();
    if (!submitted) return focusFormError(form);
    const accepted = await write.run(
      () =>
        saveIntent.submit(submitted, (input, requestId) =>
          saveIssuer(input, requestId, editing)
        ),
      async () => {
        opened = false;
        notify(editing ? 'Workload issuer saved' : 'Workload issuer added');
        await client.invalidateQueries(
          { queryKey: workloadIssuerKeys.root },
          { throwOnError: true }
        );
      },
      {
        failure: (cause, stage) =>
          (stage === 'accept'
            ? 'Issuer saved, but refreshing the view failed: '
            : '') +
          (cause instanceof Error
            ? cause.message
            : 'Unable to save the workload issuer.')
      }
    );
    if (!accepted && opened) await focusFormError(form);
  }
  async function disable(value: Issuer) {
    if (write.busy) return;
    if (
      !(await confirmAction({
        title: `Disable ${value.name}?`,
        body: 'Workloads signing in through this issuer stop being accepted at the next refresh.',
        confirmLabel: 'Disable issuer',
        danger: true
      }))
    )
      return;
    await write.run(
      () =>
        disableIntent.submit(
          { input: { ...issuerInput(value), enabled: false }, current: value },
          (attempt, attemptId) =>
            saveIssuer(attempt.input, attemptId, attempt.current)
        ),
      async (saved) => {
        if (editing?.id === value.id) open(saved);
        notify('Issuer disabled');
        await client.invalidateQueries(
          { queryKey: workloadIssuerKeys.root },
          { throwOnError: true }
        );
      },
      {
        failure: (cause, stage) =>
          (stage === 'accept'
            ? 'Issuer disabled, but refreshing the view failed: '
            : '') +
          (cause instanceof Error
            ? cause.message
            : 'Unable to disable the issuer.')
      }
    );
  }
</script>

<PageTitle title="Workload identity" />
<div class="toolbar">
  <p class="lede">
    Let workloads sign in with short-lived tokens from an issuer you trust, such
    as a cloud or CI identity provider. No shared secret is handed out.
  </p>
  {#if editable}<div class="page-actions">
      <button
        class="button button-primary"
        disabled={write.busy}
        onclick={() => open()}
      >
        New issuer
      </button>
    </div>{/if}
</div>
{#if write.error && !opened}<Callout tone="danger">
    {write.error}
  </Callout>{/if}
<QueryView
  {query}
  loading="Loading issuers…"
  errorTitle="Issuers could not be loaded"
  failure="Check your connection and try again."
  emptyWhen={(query.data ?? []).length === 0}
>
  {#snippet empty()}<EmptyState title="No workload issuers yet">
      Add an issuer to let workloads authenticate without an API key.
    </EmptyState>{/snippet}
  <ScrollTable label="Workload issuers">
    <table class="data-table">
      <thead>
        <tr>
          <th scope="col">Name</th><th scope="col">Issuer URL</th><th
            scope="col"
          >
            Status
          </th><th scope="col"><span class="sr-only">Actions</span></th>
        </tr>
      </thead>
      <tbody>
        {#each query.data ?? [] as value (value.id)}
          <tr>
            <td><strong>{value.name}</strong></td>
            <td><code class="mono">{value.issuer}</code></td>
            <td>
              <StatusBadge
                label={value.enabled ? 'Enabled' : 'Disabled'}
                tone={value.enabled ? 'success' : 'neutral'}
              />
            </td>
            <td class="row-actions">
              <button
                class="text-button"
                disabled={write.busy}
                onclick={() => open(value)}
                aria-label={`Edit ${value.name}`}
              >
                {editable ? 'Edit' : 'View'}
              </button>{#if editable && value.enabled}<button
                  class="text-button danger"
                  disabled={write.busy}
                  onclick={() => disable(value)}
                  aria-label={`Disable ${value.name}`}
                >
                  Disable
                </button>{/if}
            </td>
          </tr>
        {/each}
      </tbody>
    </table>
  </ScrollTable>
</QueryView>
{#if opened}
  <Dialog
    title={editing ? `Edit ${editing.name}` : 'New workload issuer'}
    placement="side"
    size="wide"
    onClose={() => {
      opened = false;
      pending = undefined;
    }}
  >
    <form onsubmit={submit}>
      <fieldset disabled={!editable || write.busy || Boolean(pending)}>
        <div class="form-field">
          <label for="issuer-name">Name</label><input
            id="issuer-name"
            bind:value={draft.name}
            required
            maxlength="100"
          />
        </div>
        <div class="form-field">
          <label for="issuer-url">Issuer URL</label><input
            id="issuer-url"
            type="url"
            bind:value={draft.issuer}
            required
            readonly={Boolean(editing)}
          /><small>
            Must exactly match the token’s issuer. This identity cannot be
            renamed.
          </small>
        </div>
        <div class="form-field">
          <label for="issuer-jwks">JWKS URL</label><input
            id="issuer-jwks"
            type="url"
            bind:value={draft.jwks}
            required
          /><small>
            Fetched through the separate identity egress policy. Redirects and
            unsafe destinations are refused.
          </small>
        </div>
        <label>
          <input type="checkbox" bind:checked={draft.enabled} /> Enabled
        </label>
        <div class="form-field">
          <label for="issuer-audiences">Accepted audiences</label><textarea
            id="issuer-audiences"
            bind:value={draft.audiences}
            required
            rows="2"></textarea><small>
            Separate exact audience values with commas or newlines.
          </small>
        </div>
        <fieldset class="algorithms">
          <legend>
            Signature algorithms
          </legend>{#each signatureAlgorithms as alg (alg)}<label>
              <input
                type="checkbox"
                bind:group={draft.algorithms}
                value={alg}
                aria-invalid={write.fieldErrors.algorithms ? 'true' : undefined}
                aria-describedby={write.fieldErrors.algorithms
                  ? `${id}-algorithms`
                  : undefined}
              />
              {alg}
            </label>{/each}
          {#if write.fieldErrors.algorithms}<p
              class="field-error"
              id="{id}-algorithms"
            >
              {write.fieldErrors.algorithms}
            </p>{/if}
        </fieldset>
        <FormField
          label="Maximum token lifetime (seconds)"
          error={write.fieldErrors.lifetime}
        >
          {#snippet control(attributes)}
            <input
              {...attributes}
              type="text"
              inputmode="numeric"
              bind:value={draft.lifetime}
              required
            />
          {/snippet}
        </FormField>
        <div class="form-field">
          <label for="issuer-disabled-keys">
            Disabled signing key IDs
          </label><textarea
            id="issuer-disabled-keys"
            bind:value={draft.disabledKeys}
            rows="2"></textarea>
        </div>
        <FormField
          label="Claim mappings (JSON)"
          error={write.fieldErrors.mappings}
        >
          {#snippet control(attributes)}
            <textarea
              {...attributes}
              bind:value={draft.mappings}
              rows="17"
              spellcheck="false"></textarea><small>
              Ordered mappings use exact string matches at JSON pointers. Create
              the project template and route groups first. The first matching
              mapping wins. An optional end_user_claim must contain a bounded
              machine identifier; the gateway retains only its digest.
            </small>
          {/snippet}
        </FormField>
      </fieldset>
      {#if pending}<p>
          The submitted configuration is locked for a safe retry. Reopen the
          issuer to change it.
        </p>{/if}
      {#if write.error}<p
          class="inline-problem"
          role="alert"
          tabindex="-1"
          data-error-summary
        >
          {write.error}
        </p>{/if}
      <div class="actions">
        <button
          class="button button-primary"
          disabled={write.busy || !editable}
        >
          {write.busy
            ? 'Saving…'
            : pending
              ? 'Retry save'
              : 'Save workload issuer'}
        </button><button
          type="button"
          class="button button-secondary"
          disabled={write.busy}
          onclick={() => {
            opened = false;
            pending = undefined;
          }}
        >
          Close editor
        </button>
      </div>
    </form>
  </Dialog>
{/if}

<style>
  fieldset {
    border: 0;
    padding: 0;
    margin: 0;
    display: grid;
    gap: 1rem;
  }
  .algorithms,
  .actions {
    display: flex;
    flex-wrap: wrap;
    gap: 0.75rem;
  }
  .algorithms .field-error {
    flex-basis: 100%;
  }
  .row-actions {
    display: flex;
    justify-content: flex-end;
    gap: 0.75rem;
  }
</style>
