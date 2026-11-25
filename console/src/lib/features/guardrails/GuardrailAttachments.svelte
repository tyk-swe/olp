<script lang="ts">
  import AttachmentControls from './AttachmentControls.svelte';
  import { notify } from '#lib/components/toasts.svelte.js';
  import { errorMessage } from '#lib/management/http.js';
  import type { useAction } from '#lib/editing/action.svelte.js';
  import type { useMutationIntent } from '#lib/editing/mutationIntent.svelte.js';
  import { attachPolicy as attachPolicyAPI, type Policy } from './api';
  import type { components } from '#lib/management/schema.js';

  type Attachment = components['schemas']['GuardrailAttachment'];

  let {
    action,
    refresh,
    attachIntent,
    items,
    policies,
    canEdit,
    busy,
    error = $bindable(),
    attachPolicy = $bindable(),
    scope = $bindable(),
    scopeID = $bindable(),
    order = $bindable(),
    onChanged
  }: {
    action: ReturnType<typeof useAction>;
    refresh: (current: () => boolean) => Promise<void>;
    attachIntent: ReturnType<typeof useMutationIntent>;
    items: Attachment[];
    policies: Policy[];
    canEdit: boolean;
    busy: boolean;
    error: string;
    attachPolicy: string;
    scope: Attachment['scope'];
    scopeID: string;
    order: number;
    onChanged: () => Promise<void>;
  } = $props();

  async function attach(event: SubmitEvent) {
    event.preventDefault();
    error = '';
    await action.run(
      () =>
        attachIntent.submit(
          {
            scope,
            scope_id: scope === 'installation' ? 'installation' : scopeID,
            policy_revision_id: attachPolicy,
            order
          },
          (input, requestId) => attachPolicyAPI(input, requestId)
        ),
      async (_, current) => {
        if (!current()) return;
        notify('Policy attached');
        await refresh(current);
      },
      { key: 'attach', failure: (cause) => errorMessage(cause) }
    );
    error = action.error;
  }
</script>

<section class="card stack-card">
  <h2>Inherited safeguards</h2>
  <p>
    Policies run in installation, organization, project, route or toolset, and
    key order. Each attachment adds safeguards, and narrower scopes cannot
    remove an inherited policy
  </p>
  <ul>
    {#each items as value (value.id)}<li>
        {value.scope} · {value.scope_id} · Position {value.order} · Policy revision
        {value.policy_revision_id}
        {#if value.id && value.etag}{#key value.policy_revision_id}<AttachmentControls
              value={{ ...value, id: value.id, etag: value.etag }}
              {policies}
              changed={onChanged}
            />{/key}{/if}
      </li>{/each}
  </ul>
  {#if canEdit}<form onsubmit={attach}>
      <label class="form-field">
        Policy revision<select required bind:value={attachPolicy}>
          <option value="">
            Choose a policy
          </option>{#each policies as value (value.policy_id)}<option
              value={value.revision_id}
            >
              {value.name} · Revision {value.revision}
            </option>{/each}
        </select>
      </label>
      <label class="form-field">
        Scope<select bind:value={scope}>
          {#each ['installation', 'organization', 'project', 'route', 'toolset', 'api-key'] as value (value)}<option
              {value}
            >
              {value}
            </option>{/each}
        </select>
      </label>
      {#if scope !== 'installation'}<label class="form-field">
          Scope identifier<input required bind:value={scopeID} />
        </label>{/if}
      <label class="form-field">
        Position<input type="number" min="0" required bind:value={order} />
      </label><button class="button button-primary" disabled={busy}>
        Attach policy
      </button>
    </form>{/if}
</section>

<style>
  form {
    display: grid;
    gap: 1rem;
    max-width: 52rem;
  }
  form > .button {
    justify-self: start;
  }
</style>
