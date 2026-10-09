<script lang="ts">
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { useRole } from '$lib/features/access/session/useRole.svelte';
  import {
    listIssuers,
    saveIssuer,
    issuerInput,
    type Issuer,
    type IssuerWrite
  } from './api';

  const access = useRole();
  const client = useQueryClient();
  const queryKey = ['workload-issuers'];
  const query = createQuery(() => ({
    queryKey,
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
  let requestId = $state('');
  let pending = $state<IssuerWrite>();
  let busy = $state(false);
  let error = $state('');
  let notice = $state('');
  const words = (text: string) => text.split(/[\s,]+/).filter(Boolean);

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
      value?.mappings ?? [
        {
          name: 'build',
          match: { '/repository': 'example/repository' },
          project_id: '',
          limit_template: 'worker',
          route_groups: ['generation'],
          scopes: ['inference'],
          end_user_claim: null,
          allow_provider_state: false
        }
      ],
      null,
      2
    );
    requestId = crypto.randomUUID();
    pending = undefined;
    error = '';
    notice = '';
    opened = true;
  }
  async function submit(event: SubmitEvent) {
    event.preventDefault();
    if (busy || !editable) return;
    error = '';
    try {
      if (!pending) {
        const parsed: unknown = JSON.parse(mappings);
        if (!Array.isArray(parsed) || parsed.length < 1 || parsed.length > 32)
          throw new Error('Provide one to 32 claim mappings.');
        const seconds = Number(lifetime);
        if (
          !Number.isInteger(seconds) ||
          seconds < 60 ||
          seconds > 86400 ||
          algorithms.length === 0
        )
          throw new Error(
            'Choose a signature algorithm and a maximum lifetime between 60 and 86400 seconds.'
          );
        pending = {
          name: name.trim(),
          issuer,
          jwks_url: jwks,
          enabled,
          audiences: words(audiences),
          algorithms: algorithms as IssuerWrite['algorithms'],
          max_lifetime_seconds: seconds,
          disabled_key_ids: words(disabledKeys),
          mappings: parsed as IssuerWrite['mappings']
        };
      }
      busy = true;
      const saved = await saveIssuer(pending, requestId, editing);
      open(saved);
      notice = 'Workload issuer saved.';
      await client.invalidateQueries({ queryKey });
    } catch (cause) {
      error =
        cause instanceof Error
          ? cause.message
          : 'Unable to save the workload issuer.';
    } finally {
      busy = false;
    }
  }
  async function disable(value: Issuer) {
    if (busy) return;
    busy = true;
    error = '';
    try {
      const saved = await saveIssuer(
        { ...issuerInput(value), enabled: false },
        crypto.randomUUID(),
        value
      );
      if (editing?.id === value.id) open(saved);
      notice =
        'Issuer disabled. Gateways apply the change at their next authority refresh.';
      await client.invalidateQueries({ queryKey });
    } catch (cause) {
      error =
        cause instanceof Error
          ? cause.message
          : 'Unable to disable the issuer.';
    } finally {
      busy = false;
    }
  }
</script>

<svelte:head><title>Workload identity · OLP</title></svelte:head>
<section class="card">
  <h1>Workload identity</h1>
  <p>
    Trust short-lived signed tokens from declared issuers. Claim mappings select
    a project, its limit template, route groups and inference permissions. No
    shared API secret is issued.
  </p>
  {#if error}<p role="alert">{error}</p>{/if}
  {#if notice}<p role="status">{notice}</p>{/if}
  {#if query.isPending}<p>Loading issuers…</p>{:else if query.error}<p
      role="alert"
    >
      Unable to load issuers.
    </p>
    <button class="button button-secondary" onclick={() => query.refetch()}
      >Retry</button
    >{:else}
    <ul>
      {#each query.data ?? [] as value (value.id)}
        <li>
          <strong>{value.name}</strong> — {value.enabled
            ? 'Enabled'
            : 'Disabled'}<br /><code>{value.issuer}</code>
          <div class="actions">
            <button
              class="button button-secondary"
              disabled={busy}
              onclick={() => open(value)}>Edit {value.name}</button
            >{#if editable && value.enabled}<button
                class="button button-secondary danger-button"
                disabled={busy}
                onclick={() => disable(value)}>Disable {value.name}</button
              >{/if}
          </div>
        </li>
      {:else}<li>No workload issuers are configured.</li>{/each}
    </ul>
  {/if}
  {#if editable}<button
      class="button button-primary"
      disabled={busy}
      onclick={() => open()}>Add workload issuer</button
    >{/if}
  {#if opened}
    <form onsubmit={submit}>
      <h2>{editing ? 'Edit issuer' : 'New issuer'}</h2>
      <fieldset disabled={!editable || busy || Boolean(pending)}>
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
          /><small
            >Must exactly match the token’s issuer. This identity cannot be
            renamed.</small
          >
        </div>
        <div class="form-field">
          <label for="issuer-jwks">JWKS URL</label><input
            id="issuer-jwks"
            type="url"
            bind:value={jwks}
            required
          /><small
            >Fetched through the separate identity egress policy. Redirects and
            unsafe destinations are refused.</small
          >
        </div>
        <label><input type="checkbox" bind:checked={enabled} /> Enabled</label>
        <div class="form-field">
          <label for="issuer-audiences">Accepted audiences</label><textarea
            id="issuer-audiences"
            bind:value={audiences}
            required
            rows="2"></textarea><small
            >Separate exact audience values with commas or newlines.</small
          >
        </div>
        <fieldset class="algorithms">
          <legend>Signature algorithms</legend
          >{#each ['RS256', 'ES256', 'EdDSA'] as alg (alg)}<label
              ><input type="checkbox" bind:group={algorithms} value={alg} />
              {alg}</label
            >{/each}
        </fieldset>
        <div class="form-field">
          <label for="issuer-lifetime">Maximum token lifetime (seconds)</label
          ><input
            id="issuer-lifetime"
            type="text"
            inputmode="numeric"
            bind:value={lifetime}
            required
          />
        </div>
        <div class="form-field">
          <label for="issuer-disabled-keys">Disabled signing key IDs</label
          ><textarea
            id="issuer-disabled-keys"
            bind:value={disabledKeys}
            rows="2"></textarea>
        </div>
        <div class="form-field">
          <label for="issuer-mappings">Claim mappings (JSON)</label><textarea
            id="issuer-mappings"
            bind:value={mappings}
            rows="17"
            spellcheck="false"></textarea><small
            >Ordered mappings use exact string matches at JSON pointers. Create
            the project template and route groups first. The first matching
            mapping wins. An optional end_user_claim must contain a bounded
            machine identifier; the gateway retains only its digest.</small
          >
        </div>
      </fieldset>
      {#if pending}<p>
          The submitted configuration is locked for a safe retry. Reopen the
          issuer to change it.
        </p>{/if}
      <div class="actions">
        <button class="button button-primary" disabled={busy || !editable}
          >{busy
            ? 'Saving…'
            : pending
              ? 'Retry save'
              : 'Save workload issuer'}</button
        ><button
          type="button"
          class="button button-secondary"
          disabled={busy}
          onclick={() => {
            opened = false;
            pending = undefined;
          }}>Close editor</button
        >
      </div>
    </form>
  {/if}
</section>

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
  form {
    margin-top: 1.5rem;
    border-top: 1px solid var(--border);
    padding-top: 1rem;
  }
  li {
    margin-bottom: 1rem;
    overflow-wrap: anywhere;
  }
</style>
