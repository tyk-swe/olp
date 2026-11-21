<script lang="ts">
  import type { Writable } from 'svelte/store';
  import { listServers } from './api.js';
  import type { Fields, PageState } from './grantsState.js';

  let {
    pageState,
    servers,
    working,
    setField,
    loadServer,
    saveConfiguration,
    startEnrollment
  }: {
    pageState: Writable<PageState>;
    servers: Awaited<ReturnType<typeof listServers>>;
    working: boolean;
    setField: (field: keyof Fields, value: string) => void;
    loadServer: (requested: string) => Promise<void>;
    saveConfiguration: () => Promise<void>;
    startEnrollment: () => Promise<void>;
  } = $props();
</script>

<section class="section">
  <h2>Server authorization</h2>
  <label class="form-field"
    >Registered server <select
      value={$pageState.selection.id}
      onchange={(event) => void loadServer(event.currentTarget.value)}
    >
      <option value="">Select a server</option>
      {#each servers as item (item.id)}<option value={item.id}
          >{item.name}</option
        >{/each}
    </select></label
  >
  {#if $pageState.server}
    <form
      onsubmit={(event) => {
        event.preventDefault();
        void saveConfiguration();
      }}
    >
      <label class="form-field"
        >Client ID <input
          required
          value={$pageState.fields.clientID}
          oninput={(event) => setField('clientID', event.currentTarget.value)}
        /></label
      >
      <label class="form-field"
        >Audience <input
          required
          value={$pageState.fields.audience}
          oninput={(event) => setField('audience', event.currentTarget.value)}
        /></label
      >
      <label class="form-field"
        >Scopes <input
          required
          value={$pageState.fields.scopes}
          oninput={(event) => setField('scopes', event.currentTarget.value)}
        /></label
      >
      <label class="form-field"
        >Authorization endpoint <input
          type="url"
          disabled={Boolean($pageState.fields.entraIssuer.trim())}
          required={!$pageState.fields.entraIssuer.trim()}
          value={$pageState.fields.authorizationEndpoint}
          oninput={(event) =>
            setField('authorizationEndpoint', event.currentTarget.value)}
        /></label
      >
      <label class="form-field"
        >Token endpoint <input
          type="url"
          required
          value={$pageState.fields.tokenEndpoint}
          oninput={(event) =>
            setField('tokenEndpoint', event.currentTarget.value)}
        /></label
      >
      <label class="form-field"
        >Device authorization endpoint <input
          type="url"
          disabled={Boolean($pageState.fields.entraIssuer.trim())}
          value={$pageState.fields.deviceEndpoint}
          oninput={(event) =>
            setField('deviceEndpoint', event.currentTarget.value)}
        /></label
      >
      <fieldset>
        <legend>Entra on-behalf-of authorization</legend>
        <p>
          Use a registered workload issuer and the middle-tier client ID. Target
          scopes must start with the audience. This mode requires a verified
          delegated user request and cannot combine with local delegated
          signing.
        </p>
        <label class="form-field"
          >Trusted Entra issuer <input
            type="url"
            value={$pageState.fields.entraIssuer}
            oninput={(event) =>
              setField('entraIssuer', event.currentTarget.value)}
          /></label
        >
        <label class="form-field"
          >Middle-tier client secret <input
            type="password"
            autocomplete="new-password"
            value={$pageState.fields.entraClientSecret}
            oninput={(event) =>
              setField('entraClientSecret', event.currentTarget.value)}
          /></label
        >
        <p>
          Leave the secret empty to retain the installed value. Clear the issuer
          to use browser authorization.
        </p>
      </fieldset>
      <fieldset>
        <legend>Delegated identity</legend>
        <p>
          Leave the issuer empty to disable signing. The private key stays in
          this installation.
        </p>
        <label class="form-field"
          >Delegated issuer <input
            type="url"
            value={$pageState.fields.delegatedIssuer}
            oninput={(event) =>
              setField('delegatedIssuer', event.currentTarget.value)}
          /></label
        >
        <label class="form-field"
          >Delegated audience <input
            value={$pageState.fields.delegatedAudience}
            oninput={(event) =>
              setField('delegatedAudience', event.currentTarget.value)}
          /></label
        >
        <label class="form-field"
          >PKCS8 Ed25519 signing key <textarea
            autocomplete="off"
            spellcheck="false"
            value={$pageState.fields.signingKey}
            oninput={(event) =>
              setField('signingKey', event.currentTarget.value)}
          ></textarea></label
        >
      </fieldset>
      <button class="button button-secondary" disabled={working}
        >Save authorization settings</button
      >
    </form>
    {#if $pageState.server.oauth?.entra_obo}<p>
        Entra grants are created by verified delegated user requests. Browser
        enrollment is unavailable for this server.
      </p>{/if}
    <form
      onsubmit={(event) => {
        event.preventDefault();
        void startEnrollment();
      }}
    >
      <label class="form-field"
        >API key UUID <input
          required
          value={$pageState.fields.keyID}
          oninput={(event) => setField('keyID', event.currentTarget.value)}
        /></label
      >
      <button
        class="button button-secondary"
        disabled={working ||
          !$pageState.server.oauth ||
          Boolean($pageState.server.oauth.entra_obo)}>Start key enrollment</button
      >
    </form>
  {/if}
</section>
