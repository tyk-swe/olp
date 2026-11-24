<script lang="ts">
  import Callout from '#lib/components/Callout.svelte';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { onMount } from 'svelte';
  import { errorMessage } from '#lib/management/http.js';
  import QueryView from '#lib/components/QueryView.svelte';
  import { formatDate } from '#lib/format.js';
  import { confirmAction } from '#lib/components/confirm.js';
  import { useAction } from '#lib/editing/action.svelte.js';
  import { profileKeys } from '#lib/features/identity/profile/profileKeys.js';
  import { beginIdentityReauthentication } from '#lib/features/identity/profile/api.js';
  import { reauthenticateWithPassword } from '#lib/features/identity/profile/reauthentication.js';
  import {
    getMFA,
    enrollMFA,
    manageMFA,
    removeMFA,
    recoveryMFA,
    type MFAFactor
  } from './api';
  import { confirmMFA } from '#lib/session/mfaPrompt.svelte.js';
  let {
    hasLocalPassword = false,
    hasLinkedIdentity = false
  }: { hasLocalPassword?: boolean; hasLinkedIdentity?: boolean } = $props();
  const client = useQueryClient();
  const status = createQuery(() => ({
    queryKey: profileKeys.mfa(),
    queryFn: ({ signal }) => getMFA(signal)
  }));
  let password = $state('');
  let name = $state('My authenticator');
  let kind = $state<'totp' | 'webauthn'>('totp');
  const mfa = useAction();
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
      await confirmMFA({ kind: 'challenge', challenge: await manageMFA() });
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
    await mfa.run(
      () => beginIdentityReauthentication('mfa_manage'),
      (url) => {
        window.location.assign(url);
      },
      { fallback: 'Linked identity verification could not be started.' }
    );
  }
  async function action<R>(
    work: () => Promise<R>,
    complete: (value: R, current: () => boolean) => Promise<void> | void
  ) {
    notice = '';
    codes = null;
    await mfa.run(
      async () => {
        if (!(await authorize())) return { authorized: false as const };
        return { authorized: true as const, value: await work() };
      },
      async (outcome, current) => {
        if (!outcome.authorized) return;
        await complete(outcome.value, current);
        await client.invalidateQueries({ queryKey: profileKeys.mfa() });
      },
      { fallback: 'The security operation could not be completed.' }
    );
  }
  async function enroll() {
    await action(
      () => enrollMFA(kind, name),
      async (enrollment) => {
        await confirmMFA({ kind: 'enrollment', enrollment });
        notice = 'Authenticator enrolled. Other sessions have ended.';
      }
    );
  }
  async function remove(factor: MFAFactor) {
    if (!status.data) return;
    const etag = status.data.etag;
    if (
      !(await confirmAction({
        title: `Remove ${factor.name}?`,
        body: 'You can no longer sign in with this authenticator.',
        confirmLabel: 'Remove authenticator',
        danger: true
      }))
    )
      return;
    await action(
      () => removeMFA(factor.id, etag),
      () => {
        notice = 'Authenticator removed.';
      }
    );
  }
  async function regenerate() {
    if (!status.data) return;
    const etag = status.data.etag;
    if (
      !(await confirmAction({
        title: 'Replace recovery codes?',
        body: 'Your current recovery codes stop working immediately.',
        confirmLabel: 'Replace codes',
        danger: true
      }))
    )
      return;
    await action(
      async () => (await recoveryMFA(etag)).recovery_codes,
      (result) => {
        codes = result;
        notice = 'Previous recovery codes no longer work.';
      }
    );
  }
</script>

<section class="mfa-panel" aria-labelledby="mfa-heading">
  <h2 id="mfa-heading">Multi-factor authentication</h2>
  <p>
    Protect password sign-in with an authenticator app or a security key. Linked
    identity sign-in follows your identity provider’s policy.
  </p>
  {#if mfa.error}<p class="inline-problem" role="alert">
      {mfa.error}
    </p>{/if}{#if notice}<Callout tone="success">{notice}</Callout>{/if}
  <QueryView
    query={status}
    loading="Loading authenticators…"
    failure={errorMessage(status.error, 'Authenticators could not be loaded.')}
  >
    {#if status.data}
      {#if status.data.webauthn_available === false}<p>
          Security keys need an HTTPS DNS public origin, or localhost for
          development. Authenticator apps remain available.
        </p>{/if}
      {#if status.data.required}<Callout>
          This installation requires a second factor for local sign-in. Enroll a
          replacement before removing the last factor.
        </Callout>{/if}
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
                disabled={mfa.busy ||
                  (status.data.required && status.data.factors.length === 1)}
                onclick={() => remove(factor)}>Remove {factor.name}</button
              >
            </li>{/each}
        </ul>
        <p>
          {status.data.recovery_codes_remaining} recovery codes remain. Each works
          once.
        </p>
        <button
          class="button button-secondary"
          disabled={mfa.busy}
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
          disabled={mfa.busy}
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
          disabled={mfa.busy ||
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
            >{mfa.busy ? 'Confirming…' : 'Enroll authenticator'}</button
          >
        </fieldset>
      </form>
    {/if}
  </QueryView>
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
    color: var(--foreground-muted);
  }
</style>
