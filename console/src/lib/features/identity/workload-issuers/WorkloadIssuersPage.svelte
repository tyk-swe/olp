<script lang="ts">
  import PageTitle from '#lib/shell/branding/PageTitle.svelte';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { useRole } from '#lib/session/useRole.svelte.js';
  import {
    useMutationIntent,
    usePrincipalLifetime
  } from '#lib/editing/mutationIntent.svelte.js';
  import { useAction } from '#lib/editing/action.svelte.js';
  import { captureResult } from '#lib/editing/result.js';
  import QueryView from '#lib/components/QueryView.svelte';
  import { errorMessage } from '#lib/management/http.js';
  import {
    buildIssuerWrite,
    defaultIssuerMappings,
    listIssuers,
    saveIssuer,
    issuerInput,
    type Issuer,
    type IssuerWrite
  } from './api';
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
  let editing = $state<Issuer>();
  let opened = $state(false);
  let name = $state('');
  let issuer = $state('');
  let jwks = $state('');
  let enabled = $state(true);
  let audiences = $state('');
  let algorithms = $state<string[]>(['RS256']);
  let lifetime = $state('3600');
  let disabledKeys = $state('');
  let mappings = $state('[]');
  let pending = $state<IssuerWrite>();
  const write = useAction();
  const saveIntent = useMutationIntent();
  const disableIntent = useMutationIntent();
  usePrincipalLifetime(() => {
    editing = pending = undefined;
    opened = false;
    name = issuer = jwks = audiences = disabledKeys = '';
    enabled = true;
    algorithms = ['RS256'];
    lifetime = '3600';
    mappings = '[]';
  });
  function open(value?: Issuer) {
    editing = value;
    name = value?.name ?? '';
    issuer = value?.issuer ?? '';
    jwks = value?.jwks_url ?? '';
    enabled = value?.enabled ?? true;
    audiences = value?.audiences.join('\n') ?? '';
    algorithms = [...(value?.algorithms ?? ['RS256'])];
    lifetime = String(value?.max_lifetime_seconds ?? 3600);
    disabledKeys = value?.disabled_key_ids.join('\n') ?? '';
    mappings = JSON.stringify(
      value?.mappings ?? defaultIssuerMappings,
      null,
      2
    );
    pending = undefined;
    write.error = '';
    saveIntent.reset();
    opened = true;
  }
  async function submit(event: SubmitEvent) {
    event.preventDefault();
    if (write.busy || !editable) return;
    let draft = pending;
    if (!draft) {
      const built = captureResult(() =>
        buildIssuerWrite({
          name,
          issuer,
          jwks,
          enabled,
          audiences,
          algorithms,
          lifetime,
          disabledKeys,
          mappings
        })
      );
      if (!built.ok) {
        write.error = errorMessage(
          built.error,
          'Unable to save the workload issuer.'
        );
        return;
      }
      pending = built.value;
      draft = built.value;
    }
    await write.run(
      () =>
        saveIntent.submit(draft, (input, id) => saveIssuer(input, id, editing)),
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
{#if write.error}<Callout tone="danger">{write.error}</Callout>{/if}
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
            bind:value={name}
            required
            maxlength="100"
          />
        </div>
        <div class="form-field">
          <label for="issuer-url">Issuer URL</label><input
            id="issuer-url"
            type="url"
            bind:value={issuer}
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
            bind:value={jwks}
            required
          /><small>
            Fetched through the separate identity egress policy. Redirects and
            unsafe destinations are refused.
          </small>
        </div>
        <label><input type="checkbox" bind:checked={enabled} /> Enabled</label>
        <div class="form-field">
          <label for="issuer-audiences">Accepted audiences</label><textarea
            id="issuer-audiences"
            bind:value={audiences}
            required
            rows="2"></textarea><small>
            Separate exact audience values with commas or newlines.
          </small>
        </div>
        <fieldset class="algorithms">
          <legend>
            Signature algorithms
          </legend>{#each ['RS256', 'ES256', 'EdDSA'] as alg (alg)}<label>
              <input type="checkbox" bind:group={algorithms} value={alg} />
              {alg}
            </label>{/each}
        </fieldset>
        <div class="form-field">
          <label for="issuer-lifetime">
            Maximum token lifetime (seconds)
          </label><input
            id="issuer-lifetime"
            type="text"
            inputmode="numeric"
            bind:value={lifetime}
            required
          />
        </div>
        <div class="form-field">
          <label for="issuer-disabled-keys">
            Disabled signing key IDs
          </label><textarea
            id="issuer-disabled-keys"
            bind:value={disabledKeys}
            rows="2"></textarea>
        </div>
        <div class="form-field">
          <label for="issuer-mappings">Claim mappings (JSON)</label><textarea
            id="issuer-mappings"
            bind:value={mappings}
            rows="17"
            spellcheck="false"></textarea><small>
            Ordered mappings use exact string matches at JSON pointers. Create
            the project template and route groups first. The first matching
            mapping wins. An optional end_user_claim must contain a bounded
            machine identifier; the gateway retains only its digest.
          </small>
        </div>
      </fieldset>
      {#if pending}<p>
          The submitted configuration is locked for a safe retry. Reopen the
          issuer to change it.
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
  .row-actions {
    display: flex;
    justify-content: flex-end;
    gap: 0.75rem;
  }
</style>
