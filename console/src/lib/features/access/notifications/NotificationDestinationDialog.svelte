<script lang="ts">
  import Callout from '#lib/components/Callout.svelte';
  import Dialog from '#lib/components/Dialog.svelte';
  import ProjectScopeField from '#lib/features/access/projects/ProjectScopeField.svelte';
  import { usePrincipalLifetime } from '#lib/editing/mutationIntent.svelte.js';
  import { destinationTypes, isChat } from './presentation';
  import {
    newDestinationDraft,
    type DestinationDraft
  } from './destinationDraft';

  let {
    busy,
    error,
    canManage,
    installationWide,
    onClose,
    onSubmit
  }: {
    busy: boolean;
    error: string;
    canManage: boolean;
    installationWide: boolean;
    onClose: () => void;
    onSubmit: (draft: DestinationDraft) => Promise<boolean>;
  } = $props();

  let draft = $state<DestinationDraft>(newDestinationDraft('webhook'));

  function resetDestinationForm() {
    draft = newDestinationDraft(draft.type, {
      name: '',
      url: '',
      projectId: draft.projectId
    });
  }

  usePrincipalLifetime(resetDestinationForm);

  async function submit(event: SubmitEvent) {
    event.preventDefault();
    if (!canManage || busy || !draft.name.trim() || !draft.url.trim()) return;
    if (!(await onSubmit(draft))) return;
    resetDestinationForm();
    onClose();
  }
</script>

<Dialog
  title="New destination"
  description="Where alerts are sent, such as a webhook or chat channel."
  placement="side"
  {onClose}
>
  {#if error}<Callout tone="danger">{error}</Callout>{/if}
  <form class="create-form" onsubmit={submit}>
    <fieldset class="submission-fields" disabled={busy}>
      <div class="form-grid">
        <div class="form-field">
          <label for="dest-name">Name</label><input
            id="dest-name"
            bind:value={draft.name}
            required
          />
        </div>
        <div class="form-field">
          <label for="dest-type">Type</label><select
            id="dest-type"
            value={draft.type}
            onchange={(event) => {
              const selected = destinationTypes.find(
                (option) => option.value === event.currentTarget.value
              );
              if (selected)
                draft = newDestinationDraft(selected.value, {
                  name: draft.name,
                  url: draft.url,
                  projectId: draft.projectId
                });
            }}
            >{#each destinationTypes as option (option.value)}<option
                value={option.value}>{option.label}</option
              >{/each}</select
          >
        </div>
        <div class="form-field">
          <label for="dest-url">
            {#if draft.type === 'slack' || draft.type === 'msteams' || draft.type === 'discord'}Channel
              origin URL{:else if draft.type === 'pagerduty'}Events API endpoint{:else if draft.type === 'email'}SMTP
              URL{:else}Webhook URL{/if}</label
          ><input
            id="dest-url"
            type="url"
            bind:value={draft.url}
            required
            placeholder={isChat(draft.type)
              ? 'https://hooks.slack.com'
              : draft.type === 'pagerduty'
                ? 'https://events.pagerduty.com/v2/enqueue'
                : draft.type === 'email'
                  ? 'smtps://smtp.example.com:465'
                  : 'https://hooks.example.com/olp'}
          />
        </div>
        <ProjectScopeField
          id="dest-project"
          bind:value={draft.projectId}
          unassigned={installationWide}
        />
        {#if draft.type === 'slack' || draft.type === 'msteams' || draft.type === 'discord'}
          <div class="form-field">
            <label for="dest-webhook-url">Channel webhook URL</label><input
              id="dest-webhook-url"
              bind:value={draft.webhookUrl}
              autocomplete="off"
              required
              placeholder="Full tokenized webhook URL, sealed"
            />
            <p class="section-help">
              Stored sealed; never shown again. Must share the origin URL above.
            </p>
          </div>
        {:else if draft.type === 'pagerduty'}
          <div class="form-field">
            <label for="dest-routing-key">Routing key</label><input
              id="dest-routing-key"
              bind:value={draft.routingKey}
              autocomplete="off"
              required
              placeholder="PagerDuty Events API routing key, sealed"
            />
          </div>
        {:else if draft.type === 'email'}
          <div class="form-field">
            <label for="dest-email-from">From address</label><input
              id="dest-email-from"
              bind:value={draft.from}
              required
              placeholder="alerts@example.com"
            />
          </div>
          <div class="form-field">
            <label for="dest-email-to">To addresses</label><input
              id="dest-email-to"
              bind:value={draft.to}
              required
              placeholder="ops@example.com, dev@example.com"
            />
          </div>
          <div class="form-field">
            <label for="dest-email-prefix">Subject prefix</label><input
              id="dest-email-prefix"
              bind:value={draft.subjectPrefix}
              placeholder="Optional [OLP] prefix"
            />
          </div>
          <div class="form-field">
            <label for="dest-email-ca">CA certificate (PEM)</label><textarea
              id="dest-email-ca"
              bind:value={draft.caCertificate}
              rows="3"
              placeholder="Optional pinned CA bundle"></textarea>
          </div>
          <div class="form-field">
            <label for="dest-email-username">SMTP username</label><input
              id="dest-email-username"
              bind:value={draft.username}
              autocomplete="off"
              placeholder="Optional"
            />
          </div>
          <div class="form-field">
            <label for="dest-email-password">SMTP password</label><input
              id="dest-email-password"
              type="password"
              bind:value={draft.password}
              autocomplete="new-password"
              placeholder="Optional, sealed"
            />
          </div>
        {:else if draft.type === 'webhook'}
          <div class="form-field">
            <label for="dest-secret">Signing secret</label><input
              id="dest-secret"
              bind:value={draft.secret}
              autocomplete="off"
              placeholder="Optional HMAC secret"
            />
          </div>
        {/if}
      </div>
      <div class="form-actions">
        <button
          class="button button-primary"
          type="submit"
          disabled={busy || !draft.name.trim() || !draft.url.trim()}
          >{busy ? 'Creating…' : 'Create destination'}</button
        >
      </div>
    </fieldset>
  </form>
</Dialog>
