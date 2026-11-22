<script lang="ts">
  import { onDestroy } from 'svelte';
  import { useAction } from '#lib/editing/action.svelte.js';
  import { enrollMFA, verifyMFA } from './api';
  import { webauthnCredential } from './webauthn';
  import type {
    MFAEnrollment,
    MFAVerification
  } from '#lib/session/mfaChallenge.js';
  import type { MFARequest } from '#lib/session/mfaPrompt.svelte.js';
  let {
    request,
    onComplete
  }: {
    request: MFARequest;
    onComplete: (result?: MFAVerification) => void;
  } = $props();
  const challenge = $derived(
    request.kind === 'challenge' ? request.challenge : undefined
  );
  const enrollment = $derived(
    request.kind === 'enrollment' ? request.enrollment : undefined
  );
  let started = $state<MFAEnrollment | null>(null);
  const current = $derived(started ?? enrollment);
  let method = $state<'totp' | 'recovery' | 'webauthn'>('totp');
  let code = $state('');
  let name = $state('My authenticator');
  let kind = $state<'totp' | 'webauthn'>('totp');
  const mfa = useAction();
  let codes = $state<string[] | null>(null);
  let completed = $state<MFAVerification>();
  const methods = $derived(
    current ? [current.kind] : (challenge?.methods ?? [])
  );
  const selected = $derived(methods.includes(method) ? method : methods[0]);
  const controller = new AbortController();
  onDestroy(() => controller.abort());
  async function begin() {
    await mfa.run(
      () => enrollMFA(kind, name, challenge?.challenge),
      (enrollment) => {
        started = enrollment;
      },
      { fallback: 'Enrollment could not be started.' }
    );
  }
  async function verify(event?: SubmitEvent) {
    event?.preventDefault();
    const token = current?.challenge ?? challenge?.challenge;
    if (!token || !selected) {
      mfa.error = 'Start verification again.';
      return;
    }
    await mfa.run(
      async () => {
        const credential =
          selected === 'webauthn'
            ? await webauthnCredential(
                (current?.public_key ?? challenge?.public_key)?.publicKey,
                current ? 'create' : 'get',
                controller.signal
              )
            : undefined;
        return await verifyMFA(
          {
            challenge: token,
            method: selected,
            ...(credential ? { credential } : { code })
          },
          controller.signal
        );
      },
      (result) => {
        code = '';
        if (
          result &&
          'recovery_codes' in result &&
          result.recovery_codes?.length
        ) {
          codes = result.recovery_codes;
          completed = result;
        } else onComplete(result);
      },
      { fallback: 'Verification did not succeed.' }
    );
  }
</script>

{#if mfa.error}<p class="inline-problem" role="alert">{mfa.error}</p>{/if}
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
        disabled={mfa.busy}
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
        disabled={mfa.busy}
      />
    </div>
    <button class="button button-primary" type="submit" disabled={mfa.busy}
      >{mfa.busy ? 'Starting…' : 'Start enrollment'}</button
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
          disabled={mfa.busy}
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
          disabled={mfa.busy}
        />
      </div>{:else}<p>
        Use your security key or passkey and confirm its PIN or device
        verification.
      </p>{/if}
    <button class="button button-primary" type="submit" disabled={mfa.busy}
      >{mfa.busy
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
