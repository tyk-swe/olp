<script lang="ts">
  import type { PublicationForm } from './form.js';

  let {
    form = $bindable(),
    canTrust,
    credential = $bindable(),
    removeCredential = $bindable(),
    entraClientSecret = $bindable()
  }: {
    form: PublicationForm;
    canTrust: boolean | undefined;
    credential: string;
    removeCredential: boolean;
    entraClientSecret: string;
  } = $props();
</script>

<label class="form-field"
  >Protocol contract <select bind:value={form.protocol}
    ><option value="a2a-1.0.0">A2A 1.0.0</option><option value="a2a-0.3.0"
      >A2A 0.3.0</option
    ></select
  ></label
><label class="form-field"
  >Approved endpoint <input
    type="url"
    bind:value={form.endpoint}
    required
    disabled={!canTrust}
  /></label
><label class="form-field"
  >Reviewed agent-card SHA-256 <input
    bind:value={form.cardDigest}
    required
    disabled={!canTrust}
  /></label
><label class="form-field"
  >Pricing revision UUID <input
    bind:value={form.pricingRevision}
    required
  /></label
><label class="form-field"
  >Finite maximum cost <input bind:value={form.maxCost} required /></label
><label class="form-field"
  >Price per message <input
    inputmode="decimal"
    bind:value={form.messagePrice}
  /></label
>
<p>Leave blank to disable sending, or enter 0 for free messages.</p>
<label class="form-field"
  >Declared maximum remote work duration in seconds <input
    inputmode="numeric"
    pattern="[0-9]+"
    bind:value={form.remoteWorkBoundSeconds}
  /></label
>
<p>
  The agent must enforce this bound on its own work. Budgeted calls require a
  bound.
</p>
<label class="form-field"
  >Maximum task continuations <input
    inputmode="numeric"
    pattern="[0-9]+"
    bind:value={form.maxTaskContinuations}
    placeholder="32"
  /></label
>
<label class="form-field"
  >Task deadline in seconds <input
    inputmode="numeric"
    pattern="[0-9]+"
    bind:value={form.taskDeadlineSeconds}
    placeholder="600"
  /></label
>
<p>Continuations are limited to 128 and the deadline to 3600 seconds.</p>
<label class="form-field"
  >Pinned toolset revision UUIDs <textarea bind:value={form.toolsetRevisions}
  ></textarea></label
><label class="form-field"
  >Pinned route revision UUIDs <textarea bind:value={form.routeRevisions}
  ></textarea></label
><label class="form-field"
  >Replacement bearer credential <input
    type="password"
    autocomplete="new-password"
    bind:value={credential}
    disabled={!canTrust || form.oauthEnabled}
  /></label
><label class="check-field"
  ><input
    type="checkbox"
    bind:checked={removeCredential}
    disabled={!canTrust || form.oauthEnabled}
  /> Remove credential</label
>
<fieldset disabled={!canTrust}>
  <legend>Personal OAuth authorization</legend>
  <label class="check-field"
    ><input type="checkbox" bind:checked={form.oauthEnabled} /> Use OAuth grants</label
  >
  {#if form.oauthEnabled}
    <p>
      OAuth replaces any saved bearer credential. Publishing requires a publicly
      readable agent card for publication and discovery. Task requests use the
      caller's grant.
    </p>
    <label class="check-field"
      ><input type="checkbox" bind:checked={form.oauthEntra} /> Use Entra on-behalf-of</label
    >
    {#if form.oauthEntra}
      <label class="form-field"
        >Workload issuer <input
          type="url"
          bind:value={form.oauthEntraIssuer}
          required
        /></label
      >
      <label class="form-field"
        >Replacement Entra client secret <input
          type="password"
          autocomplete="new-password"
          bind:value={entraClientSecret}
        /></label
      >
      <p>
        Supply the client secret when enabling Entra or changing its issuer,
        client ID, or token endpoint.
      </p>
    {/if}
    <label class="form-field"
      >Client ID <input bind:value={form.oauthClientID} required /></label
    >
    <label class="form-field"
      >Audience <input bind:value={form.oauthAudience} required /></label
    >
    <label class="form-field"
      >Scopes <input bind:value={form.oauthScopes} required /></label
    >
    {#if !form.oauthEntra}<label class="form-field"
        >Authorization endpoint <input
          type="url"
          bind:value={form.oauthAuthorizationEndpoint}
          required
        /></label
      >{/if}
    <label class="form-field"
      >Token endpoint <input
        type="url"
        bind:value={form.oauthTokenEndpoint}
        required
      /></label
    >
    {#if !form.oauthEntra}<label class="form-field"
        >Device authorization endpoint <input
          type="url"
          bind:value={form.oauthDeviceEndpoint}
        /></label
      >{/if}
    <p>
      To change the agent endpoint, remove OAuth and publish the new endpoint
      and card first. Then configure OAuth again.
    </p>
  {/if}
</fieldset>
<p>
  Destination, card, and credential trust changes require installation access.
  Existing credentials are never displayed.
</p>
