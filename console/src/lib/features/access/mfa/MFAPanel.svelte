<script lang="ts">
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { onMount } from 'svelte';
  import { errorMessage } from '$lib/api/http';
  import { formatDate } from '$lib/format';
  import {
    beginIdentityReauthentication,
    reauthenticateWithPassword
  } from '$lib/features/access/profile/api';
  import { getMFA, enrollMFA, manageMFA, removeMFA, recoveryMFA } from './api';
  import { confirmMFA } from './prompt.svelte';
  let {
    hasLocalPassword = false,
    hasLinkedIdentity = false
  }: { hasLocalPassword?: boolean; hasLinkedIdentity?: boolean } = $props();
  const client = useQueryClient();
  const key = ['profile', 'mfa'];
  const status = createQuery(() => ({
    queryKey: key,
    queryFn: ({ signal }) => getMFA(signal)
  }));
  let password = $state('');
  let name = $state('My authenticator');
  let kind = $state<'totp' | 'webauthn'>('totp');
  let busy = $state(false);
  let error = $state('');
  let notice = $state('');
  let codes = $state<string[] | null>(null);
  let oidcConfirmed = $state(false);
  onMount(() => {
    oidcConfirmed =
      new URLSearchParams(window.location.search).get('reauthenticated') ===
      'mfa_manage';
  });
  $effect(() => {
    if (
      status.data?.webauthn_available !== false &&
      status.data?.factors.some((f) => f.kind === 'totp') &&
      kind === 'totp'
    )
      kind = 'webauthn';
  });
  async function authorize() {
    if (status.data?.factors.length) {
      await confirmMFA({ challenge: await manageMFA() });
      return true;
    }
    if (oidcConfirmed) {
      oidcConfirmed = false;
      return true;
    }
    if (hasLocalPassword) {
      await reauthenticateWithPassword(password, 'mfa_manage');
      password = '';
      return true;
    }
    if (hasLinkedIdentity) {
      window.location.assign(await beginIdentityReauthentication('mfa_manage'));
      return false;
    }
    throw new Error('Confirm a password or linked identity before enrollment.');
  }
  async function confirmLinkedIdentity() {
    if (busy) return;
    busy = true;
    error = '';
    try {
      window.location.assign(await beginIdentityReauthentication('mfa_manage'));
    } catch (e) {
      error = errorMessage(
        e,
        'Linked identity verification could not be started.'
      );
    } finally {
      busy = false;
    }
  }
  async function action(work: () => Promise<void>) {
    if (busy) return;
    busy = true;
    error = '';
    notice = '';
    codes = null;
    try {
      if (!(await authorize())) return;
      await work();
      await client.invalidateQueries({ queryKey: key });
    } catch (e) {
      error = errorMessage(e, 'The security operation could not be completed.');
    } finally {
      busy = false;
    }
  }
  async function enroll() {
    await action(async () => {
      await confirmMFA({ enrollment: await enrollMFA(kind, name) });
      notice = 'Authenticator enrolled. Other sessions have ended.';
    });
  }
  async function remove(id: string) {
    if (!status.data) return;
    const etag = status.data.etag;
    await action(async () => {
      await removeMFA(id, etag);
      notice = 'Authenticator removed.';
    });
  }
  async function regenerate() {
    if (!status.data) return;
    const etag = status.data.etag;
    await action(async () => {
      codes = (await recoveryMFA(etag)).recovery_codes;
      notice = 'Previous recovery codes no longer work.';
    });
  }
</script>

<section class="mfa-panel" aria-labelledby="mfa-heading">
  <h2 id="mfa-heading">Multi-factor authentication</h2>
  <p>
    Protect password sign-in with an authenticator app or a security key. Linked
    identity sign-in follows your identity provider’s policy.
  </p>
  {#if error}<p class="form-alert" role="alert">{error}</p>{/if}{#if notice}<p
      class="notice notice-success"
      role="status"
    >
      {notice}
    </p>{/if}
  {#if status.isPending}<p role="status">
      Loading authenticators…
    </p>{:else if status.isError}<p class="form-alert" role="alert">
      {errorMessage(status.error, 'Authenticators could not be loaded.')}
    </p>
    <button class="button button-secondary" onclick={() => status.refetch()}
      >Retry</button
    >
  {:else if status.data}
    {#if status.data.webauthn_available === false}<p>
        Security keys need an HTTPS DNS public origin, or localhost for
        development. Authenticator apps remain available.
      </p>{/if}
    {#if status.data.required}<p class="notice notice-info">
        This installation requires a second factor for local sign-in. Enroll a
        replacement before removing the last factor.
      </p>{/if}
    {#if status.data.factors.length}<ul>
        {#each status.data.factors as factor (factor.id)}<li>
            <div>
              <strong>{factor.name}</strong><span
                >{factor.kind === 'totp'
                  ? 'Authenticator app'
                  : 'Security key or passkey'} · Added {formatDate(
                  factor.created_at
                )}</span
              >
            </div>
            <button
              class="button button-secondary"
              disabled={busy ||
                (status.data.required && status.data.factors.length === 1)}
              onclick={() => remove(factor.id)}>Remove {factor.name}</button
            >
          </li>{/each}
      </ul>
      <p>
        {status.data.recovery_codes_remaining} recovery codes remain. Each works once.
      </p>
      <button
        class="button button-secondary"
        disabled={busy}
        onclick={regenerate}>Replace recovery codes</button
      >{/if}
    {#if codes}<div class="form-field">
        <label for="profile-recovery-codes"
          >New recovery codes — save privately</label
        ><textarea
          id="profile-recovery-codes"
          readonly
          rows="10"
          value={codes.join('\n')}></textarea>
      </div>
      <button
        class="button button-secondary"
        onclick={() => {
          codes = null;
        }}>I saved these codes</button
      >{/if}
    {#if !status.data.factors.length && hasLinkedIdentity && !oidcConfirmed}
      <button
        type="button"
        class="button button-secondary"
        disabled={busy}
        onclick={confirmLinkedIdentity}
        >Verify linked identity for enrollment</button
      >
    {/if}
    <form
      onsubmit={(e) => {
        e.preventDefault();
        void enroll();
      }}
    >
      <fieldset
        disabled={busy ||
          status.data.factors.length >= 10 ||
          (status.data.webauthn_available === false &&
            status.data.factors.some((f) => f.kind === 'totp'))}
      >
        <legend>Enroll an authenticator</legend>
        <div class="form-field">
          <label for="profile-mfa-kind">Authenticator type</label><select
            id="profile-mfa-kind"
            bind:value={kind}
            ><option
              value="totp"
              disabled={status.data.factors.some((f) => f.kind === 'totp')}
              >Authenticator app</option
            ><option
              value="webauthn"
              disabled={status.data?.webauthn_available === false}
              >Security key or passkey</option
            ></select
          >
        </div>
        <div class="form-field">
          <label for="profile-mfa-name">Authenticator name</label><input
            id="profile-mfa-name"
            bind:value={name}
            maxlength="100"
            required
          />
        </div>
        {#if !status.data.factors.length && hasLocalPassword && !oidcConfirmed}<div
            class="form-field"
          >
            <label for="profile-mfa-password"
              >Current password for enrollment</label
            ><input
              id="profile-mfa-password"
              type="password"
              autocomplete="current-password"
              required
              bind:value={password}
            />
          </div>{/if}
        <button class="button button-primary" type="submit"
          >{busy ? 'Confirming…' : 'Enroll authenticator'}</button
        >
      </fieldset>
    </form>
  {/if}
</section>

<style>
  .mfa-panel {
    display: grid;
    gap: 1rem;
    border-top: 1px solid var(--border);
    padding-top: 1.5rem;
    margin-top: 1.5rem;
  }
  h2 {
    font-size: 1.2rem;
  }
  fieldset {
    display: grid;
    gap: 1rem;
    border: 0;
    padding: 0;
  }
  legend {
    font-weight: 600;
    margin-bottom: 1rem;
  }
  ul {
    padding: 0;
    list-style: none;
    display: grid;
    gap: 0.75rem;
  }
  li {
    display: flex;
    gap: 1rem;
    justify-content: space-between;
    align-items: center;
    flex-wrap: wrap;
  }
  li span {
    display: block;
    color: var(--text-muted);
  }
</style>
