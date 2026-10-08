<script lang="ts">
  import { onDestroy } from 'svelte';
  import { errorMessage } from '$lib/api/http';
  import {
    enrollMFA,
    verifyMFA,
    type MFAChallenge,
    type MFAEnrollment,
    type MFAVerification
  } from './api';
  let {
    challenge,
    enrollment,
    onComplete
  }: {
    challenge?: MFAChallenge;
    enrollment?: MFAEnrollment;
    onComplete: (result?: MFAVerification) => void;
  } = $props();
  let started = $state<MFAEnrollment | null>(null);
  const current = $derived(started ?? enrollment);
  let method = $state<'totp' | 'recovery' | 'webauthn'>('totp');
  let code = $state('');
  let name = $state('My authenticator');
  let kind = $state<'totp' | 'webauthn'>('totp');
  let busy = $state(false);
  let error = $state('');
  let codes = $state<string[] | null>(null);
  let completed = $state<MFAVerification>();
  const methods = $derived(
    current ? [current.kind] : (challenge?.methods ?? [])
  );
  const selected = $derived(methods.includes(method) ? method : methods[0]);
  const controller = new AbortController();
  onDestroy(() => controller.abort());
  async function begin() {
    busy = true;
    error = '';
    try {
      started = await enrollMFA(kind, name, challenge?.challenge);
    } catch (e) {
      error = errorMessage(e, 'Enrollment could not be started.');
    } finally {
      busy = false;
    }
  }
  async function verify(event?: SubmitEvent) {
    event?.preventDefault();
    if (busy) return;
    busy = true;
    error = '';
    try {
      const token = current?.challenge ?? challenge?.challenge;
      if (!token) throw new Error('Start verification again.');
      let credential: Record<string, unknown> | undefined;
      if (selected === 'webauthn') {
        if (
          typeof PublicKeyCredential === 'undefined' ||
          !window.isSecureContext
        )
          throw new Error(
            'Security keys require a supported browser and a secure origin.'
          );
        const raw = (current?.public_key ?? challenge?.public_key)?.publicKey;
        const result = current
          ? await navigator.credentials.create({
              publicKey: PublicKeyCredential.parseCreationOptionsFromJSON(
                raw as PublicKeyCredentialCreationOptionsJSON
              ),
              signal: controller.signal
            })
          : await navigator.credentials.get({
              publicKey: PublicKeyCredential.parseRequestOptionsFromJSON(
                raw as PublicKeyCredentialRequestOptionsJSON
              ),
              signal: controller.signal
            });
        if (!(result instanceof PublicKeyCredential))
          throw new Error('No security-key response was received.');
        credential = result.toJSON() as unknown as Record<string, unknown>;
      }
      const result = await verifyMFA(
        {
          challenge: token,
          method: selected as 'totp' | 'recovery' | 'webauthn',
          ...(credential ? { credential } : { code })
        },
        controller.signal
      );
      code = '';
      if (
        result &&
        'recovery_codes' in result &&
        result.recovery_codes?.length
      ) {
        codes = result.recovery_codes;
        completed = result;
      } else onComplete(result);
    } catch (e) {
      error = errorMessage(e, 'Verification did not succeed.');
    } finally {
      busy = false;
    }
  }
</script>

{#if error}<p class="form-alert" role="alert">{error}</p>{/if}
{#if codes}
  <h3>Save your recovery codes</h3>
  <p>
    Each code works once. Store them somewhere private before continuing. They
    are shown only now.
  </p>
  <div class="form-field">
    <label for="mfa-recovery-output">Recovery codes</label><textarea
      id="mfa-recovery-output"
      rows="10"
      readonly
      value={codes.join('\n')}></textarea>
  </div>
  <button
    class="button button-primary"
    type="button"
    onclick={() => onComplete(completed)}>I saved my recovery codes</button
  >
{:else if challenge?.enrollment_required && !current}
  <p>
    This installation requires a second factor before local sign-in completes.
    Enroll an authenticator app or a security key.
  </p>
  <form
    onsubmit={(e) => {
      e.preventDefault();
      void begin();
    }}
  >
    <div class="form-field">
      <label for="mfa-kind">Authenticator</label><select
        id="mfa-kind"
        bind:value={kind}
        disabled={busy}
        ><option value="totp">Authenticator app</option><option
          value="webauthn"
          disabled={challenge.webauthn_available === false}
          >Security key or passkey</option
        ></select
      >
    </div>
    <div class="form-field">
      <label for="mfa-name">Authenticator name</label><input
        id="mfa-name"
        maxlength="100"
        required
        bind:value={name}
        disabled={busy}
      />
    </div>
    <button class="button button-primary" type="submit" disabled={busy}
      >{busy ? 'Starting…' : 'Start enrollment'}</button
    >
  </form>
{:else}
  {#if current?.kind === 'totp'}<p>
      Scan this code with your authenticator app, then enter its current
      six-digit code.
    </p>
    <img
      class="qr"
      src={current.qr_code}
      alt="Authenticator enrollment QR code"
      width="256"
      height="256"
    />
    <div class="form-field">
      <label for="mfa-seed">Manual setup key</label><input
        id="mfa-seed"
        readonly
        value={current.secret}
      />
    </div>{/if}
  <form onsubmit={verify}>
    {#if methods.length > 1}<div class="form-field">
        <label for="mfa-method">Verification method</label><select
          id="mfa-method"
          bind:value={method}
          disabled={busy}
          >{#each methods as option (option)}<option value={option}
              >{option === 'totp'
                ? 'Authenticator app'
                : option === 'recovery'
                  ? 'Recovery code'
                  : 'Security key or passkey'}</option
            >{/each}</select
        >
      </div>{/if}
    {#if selected !== 'webauthn'}<div class="form-field">
        <label for="mfa-code"
          >{selected === 'recovery'
            ? 'Recovery code'
            : 'Authentication code'}</label
        ><input
          id="mfa-code"
          data-autofocus
          bind:value={code}
          required
          maxlength="128"
          inputmode={selected === 'totp' ? 'numeric' : 'text'}
          autocomplete="one-time-code"
          disabled={busy}
        />
      </div>{:else}<p>
        Use your security key or passkey and confirm its PIN or device
        verification.
      </p>{/if}
    <button class="button button-primary" type="submit" disabled={busy}
      >{busy
        ? 'Verifying…'
        : selected === 'webauthn'
          ? 'Use security key'
          : 'Verify'}</button
    >
  </form>
{/if}

<style>
  form {
    display: grid;
    gap: 1rem;
  }
  p {
    margin-block: 1rem;
  }
  .qr {
    max-width: 100%;
    height: auto;
  }
  h3 {
    font-size: 1.1rem;
  }
</style>
