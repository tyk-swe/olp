<script lang="ts">
  import Advanced from '#lib/components/Advanced.svelte';
  import NamedLimitFields from '../budgets/NamedLimitFields.svelte';
  import AttributionFields from '../attribution/AttributionFields.svelte';
  import PolicyFields from '../end-users/PolicyFields.svelte';
  import type { ApiKeyFormState } from './apiKeyPolicy.js';
  import type { useServiceCapabilities } from '#lib/session/serviceCapabilities.svelte.js';
  import type { listRoutes } from '#lib/features/routes/index.js';

  let {
    form,
    errors,
    canManage,
    busy,
    publicationBlocked,
    services,
    routes,
    toggle,
    touch
  }: {
    form: ApiKeyFormState;
    errors: Record<string, string>;
    canManage: boolean;
    busy: string;
    publicationBlocked: boolean;
    services: ReturnType<typeof useServiceCapabilities>;
    routes: {
      isPending: boolean;
      isError: boolean;
      error: unknown;
      data?: Awaited<ReturnType<typeof listRoutes>>;
      refetch: () => Promise<unknown>;
    };
    toggle: (list: string[], value: string, checked: boolean) => string[];
    touch: () => void;
  } = $props();
</script>

<section aria-labelledby="scope-heading">
  <h2 id="scope-heading">What this key can reach</h2>
  <fieldset class="checks">
    <legend>Scopes</legend
    >{#each [['inference', 'Inference requests'], ['models_read', 'Model listing'], ['guardrails', 'Guardrail evaluation'], ['tools', 'Toolset access'], ['agents', 'Agent access'], ['traces_write', 'Trace ingestion'], ['delegate', 'Delegation consent']] as scope (scope[0])}<label
        ><input
          type="checkbox"
          checked={form.scopes.includes(scope[0])}
          disabled={!canManage}
          onchange={(event) =>
            (form.scopes = toggle(
              form.scopes,
              scope[0],
              event.currentTarget.checked
            ))}
        />
        {scope[1]}</label
      >{/each}
  </fieldset>
  <fieldset class="checks routes">
    <legend>Routes</legend>
    <p class="section-help">
      {services.gatewayAvailable
        ? 'Leave every route unchecked to allow all current and future routes.'
        : 'Leave blank to allow all current and future routes.'}
    </p>
    {#if services.pending}<span class="inline-status" role="status"
        >Loading routes…</span
      >
    {:else if !services.gatewayAvailable}
      <label for="allowed-routes">Route slugs (comma separated)</label>
      <input
        id="allowed-routes"
        value={form.allowedRoutes.join(', ')}
        disabled={!canManage}
        onchange={(event) => {
          form.allowedRoutes = [
            ...new Set(
              event.currentTarget.value
                .split(',')
                .map((slug) => slug.trim())
                .filter(Boolean)
            )
          ];
          touch();
        }}
      />
      <span>Save planned route restrictions. Routing is not available yet.</span
      >
    {:else if routes.isPending}<span class="inline-status" role="status"
        >Loading routes…</span
      >{:else if routes.isError}<span class="inline-problem" role="alert"
        >Routes are unavailable, so route restrictions cannot be reviewed.
        <button
          class="text-button"
          type="button"
          onclick={() => routes.refetch()}>Retry</button
        ></span
      >{:else}{#each routes.data ?? [] as route (route.id)}<label
          ><input
            type="checkbox"
            checked={form.allowedRoutes.includes(route.slug)}
            disabled={!canManage}
            onchange={(event) =>
              (form.allowedRoutes = toggle(
                form.allowedRoutes,
                route.slug,
                event.currentTarget.checked
              ))}
          />

          <code>{route.slug}</code>
        </label>
      {/each}{#if !routes.data?.length}
        <span>No routes are configured yet.</span>
      {/if}
    {/if}
  </fieldset>
  <Advanced label="Advanced access rules" open={Boolean(errors.allowedCIDRs)}>
    <div class="advanced-fields">
      <div class="form-field">
        <label for="key-toolsets">Allowed published toolset UUIDs</label><input
          id="key-toolsets"
          bind:value={form.allowedToolsets}
          disabled={!canManage}
        /><small
          >Comma-separated; an empty list denies all toolsets. Route groups
          grant no tool access.</small
        >
      </div>
      <div class="form-field">
        <label for="key-agents">Allowed published agent UUIDs</label><input
          id="key-agents"
          bind:value={form.allowedAgents}
          disabled={!canManage}
        /><small>Comma-separated; an empty list denies all agents.</small>
      </div>
      <fieldset class="checks">
        <legend>Provider state</legend>
        <label
          ><input
            type="checkbox"
            bind:checked={form.allowProviderState}
            disabled={!canManage}
          />
          Allow provider-retained state</label
        >
        <p class="section-help">
          Permits stored Responses, previous_response_id chains, and lifecycle
          access. Provider state may retain user content at the upstream
          provider.
        </p>
      </fieldset>
      <fieldset class="checks">
        <legend>Response metadata</legend>
        <label
          ><input
            type="checkbox"
            bind:checked={form.forbidResponseCache}
            disabled={!canManage}
          /> Forbid response caching for this key</label
        >
        <label
          ><input
            type="checkbox"
            bind:checked={form.responseMetadata}
            disabled={!canManage}
          />
          Add gateway metadata headers</label
        >
        <p class="section-help">
          Adds X-OLP-Attempts, X-OLP-Route-Revision, X-OLP-Provider and
          X-OLP-Cost to inference responses. The gateway names the serving
          provider only with this opt-in; an upstream's own error message is
          relayed as it is.
        </p>
      </fieldset>
      <fieldset class="checks">
        <legend>End-user identity</legend>
        <div class="form-field full">
          <label for="key-end-user-source">Identifier source</label>
          <select
            id="key-end-user-source"
            bind:value={form.endUserSource}
            disabled={!canManage}
            aria-describedby="key-end-user-help"
          >
            <option value="">Not required</option>
            <option value="header">X-OLP-End-User header</option>
            <option value="native">Native JSON field</option>
          </select>
          <small id="key-end-user-help">
            Requires a machine token of up to 128 characters. Native fields are
            OpenAI safety_identifier (preferred) or user, and Anthropic
            metadata.user_id. Other surfaces require the header. Reports retain
            only a project-scoped digest; the header is never sent upstream.
          </small>
        </div>
      </fieldset>
      <PolicyFields
        bind:form={form.endUserPolicy}
        prefix="key-end-user"
        disabled={!canManage || Boolean(busy) || publicationBlocked}
      />
      <div class="form-field full">
        <label for="key-allowed-cidrs">Allowed client networks</label>
        <textarea
          id="key-allowed-cidrs"
          rows="3"
          bind:value={form.allowedCIDRs}
          disabled={!canManage || Boolean(busy)}
          aria-invalid={!!errors.allowedCIDRs}
          aria-describedby={errors.allowedCIDRs
            ? 'key-allowed-cidrs-help key-allowed-cidrs-error'
            : 'key-allowed-cidrs-help'}
          placeholder="192.0.2.0/24
    2001:db8::/32"></textarea>

        <small id="key-allowed-cidrs-help"
          >One IPv4 or IPv6 CIDR range per line. Leave empty to allow any client
          address. Forwarded addresses are accepted only through configured
          trusted proxies.</small
        >

        {#if errors.allowedCIDRs}
          <p id="key-allowed-cidrs-error" class="field-error" role="alert">
            {errors.allowedCIDRs}
          </p>{/if}
      </div>
      <div class="form-field full">
        <label for="key-route-groups">Allowed route groups</label>
        <input
          id="key-route-groups"
          bind:value={form.allowedRouteGroups}
          disabled={!canManage || Boolean(busy)}
          aria-describedby="key-route-groups-help"
        />
        <small id="key-route-groups-help"
          >Existing group names in the selected project, separated by commas.
          Explicit routes and group members are combined. Both lists empty allow
          every route in the project; an empty or missing referenced group
          grants nothing.</small
        >
      </div>
      <div class="form-field full">
        <label for="rotation-interval">Rotation reminder interval (days)</label>
        <input
          id="rotation-interval"
          inputmode="numeric"
          bind:value={form.rotationIntervalDays}
          placeholder="No scheduled reminder"
          disabled={!canManage || Boolean(busy)}
          aria-describedby="rotation-interval-help"
        />
        <small id="rotation-interval-help"
          >Declare 1–3650 days from creation or the last rotation. Subscribe to
          key.expiring in Notifications to receive reminders. Secrets rotate
          only when explicitly requested.</small
        >
      </div>
      <div class="form-field full">
        <label for="key-limit-template">Limit template</label><input
          id="key-limit-template"
          bind:value={form.limitTemplate}
          maxlength="100"
          disabled={!canManage || Boolean(busy)}
        /><small
          >Optional template in this key’s project. The stricter template or
          inline limit applies. Manage templates in Project policies.</small
        >
      </div>
      <NamedLimitFields
        kind="route"
        bind:form={form.routeLimits}
        disabled={!canManage || Boolean(busy) || publicationBlocked}
      />
      <AttributionFields
        bind:form={form.attributionPolicy}
        prefix="key-attribution"
        disabled={!canManage || Boolean(busy)}
      />
      <fieldset class="checks">
        <legend>Allowed attribution keys</legend>
        <p>
          Callers may attach short machine-token labels under these keys in the
          X-OLP-Attribution header. Leave blank to reject every label.
        </p>
        <label for="allowed-attribution-keys"
          >Attribution keys (comma separated)</label
        >
        <input
          id="allowed-attribution-keys"
          value={form.allowedAttributionKeys.join(', ')}
          disabled={!canManage}
          placeholder="team, env"
          onchange={(event) => {
            form.allowedAttributionKeys = [
              ...new Set(
                event.currentTarget.value
                  .split(',')
                  .map((key) => key.trim())
                  .filter(Boolean)
              )
            ];
            touch();
          }}
        />
      </fieldset>
    </div>
  </Advanced>
</section>

<style>
  h2 {
    margin: 0 0 0.75rem;
    font-size: var(--text-body);
    font-weight: 500;
    letter-spacing: -0.02em;
  }
  section {
    padding-top: 1.5rem;
    border-top: 1px solid var(--border-hairline);
  }
  .advanced-fields {
    display: grid;
    gap: 1.25rem;
    padding-top: 1rem;
  }
  .checks {
    display: flex;
    flex-wrap: wrap;
    gap: 0.65rem 1rem;
    margin: 0;
    padding: 0;
    border: 0;
  }
  .checks legend {
    width: 100%;
    margin-bottom: 0.4rem;
    font-weight: 500;
  }
  .checks label {
    display: inline-flex;
    min-height: 2.75rem;
    align-items: center;
    gap: 0.45rem;
  }
  .checks.routes {
    display: grid;
    margin-top: 1rem;
    padding: 1rem;
    border-radius: var(--radius-control);
    background: var(--surface-raised);
  }
  code {
    font-size: var(--text-caption);
  }
</style>
