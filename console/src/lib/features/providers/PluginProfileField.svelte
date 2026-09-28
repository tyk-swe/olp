<script lang="ts">
  import { createQuery } from '@tanstack/svelte-query';
  import { resolve } from '$app/paths';
  import type { FieldIssue } from '$lib/api/http';
  import { shortDigest } from '$lib/features/plugins/api';
  import NativeValueField from './NativeValueField.svelte';
  import {
    listProviderProfiles,
    pluginOptionFields,
    pluginProfileGroups,
    type ProviderProfilePlugin
  } from './profiles';
  import {
    selectPluginProfile,
    type ProviderEditValues
  } from './providerEditor';

  let {
    values,
    idPrefix,
    disabled = false,
    issues = [],
    onChange
  }: {
    values: ProviderEditValues;
    idPrefix: string;
    disabled?: boolean;
    /** Field issues the server reported, shown at the options they name. */
    issues?: FieldIssue[];
    onChange?: () => void;
  } = $props();

  const profiles = createQuery(() => ({
    queryKey: ['provider-profiles'],
    queryFn: ({ signal }) => listProviderProfiles(signal)
  }));
  const groups = $derived(pluginProfileGroups(profiles.data ?? []));
  const pinned = $derived(
    values.profileId ? `${values.profileId}@${values.profileRevision}` : ''
  );
  const selected = $derived(pluginProfile(pinned));

  const options = $derived(pluginOptionFields(selected));

  /** The server's issue with an option's value, if any. */
  function optionIssue(name: string) {
    return (
      issues.find(
        (issue) =>
          issue.field === `configuration.options.plugin_options.${name}`
      )?.message ?? ''
    );
  }

  /** The catalogue's plugin profile pinned as `id@revision`. */
  function pluginProfile(pin: string) {
    return profiles.data?.find(
      (profile) =>
        profile.kind === 'plugin' && `${profile.id}@${profile.revision}` === pin
    );
  }

  /** Options may place the address, which the server sets again on save. */
  function changeOption() {
    values.endpoint = '';
    onChange?.();
  }

  /** Names a plugin build: its name, version and short digest. */
  function build(plugin: ProviderProfilePlugin) {
    return `${plugin.name} ${plugin.version} · digest ${shortDigest(plugin.digest)}`;
  }

  function choose(value: string) {
    selectPluginProfile(values, pluginProfile(value));
    onChange?.();
  }
</script>

<div class="form-field full">
  <label for={`${idPrefix}-plugin-profile`}>Plugin profile</label>
  <select
    id={`${idPrefix}-plugin-profile`}
    aria-describedby={`${idPrefix}-plugin-profile-help`}
    value={pinned}
    onchange={(event) => choose(event.currentTarget.value)}
    disabled={disabled ||
      !values.document?.fieldsAvailable ||
      profiles.isPending ||
      profiles.isError ||
      (!groups.length && !pinned)}
    required
  >
    <option value="" disabled>Choose a plugin profile</option>
    {#if pinned && !selected}<option value={pinned}
        >{values.profileId} · digest {shortDigest(
          values.profileRevision ?? ''
        )}</option
      >{/if}
    {#each groups as group (group.plugin.digest)}<optgroup
        label={build(group.plugin)}
      >
        {#each group.profiles as profile (profile.id)}<option
            value={`${profile.id}@${profile.revision}`}
            >{`${profile.label} · ${build(group.plugin)}`}</option
          >{/each}
      </optgroup>{/each}
  </select>
  <small id={`${idPrefix}-plugin-profile-help`}
    >The provider pins the plugin build by its digest. Moving to another build
    of the plugin is a new revision.</small
  >
</div>
{#if profiles.isError}<div class="inline-problem full" role="alert">
    The plugin profiles could not be loaded. <button
      class="button button-secondary"
      type="button"
      onclick={() => profiles.refetch()}>Retry</button
    >
  </div>
{:else if profiles.isSuccess && !groups.length}<p class="plugin-note full">
    No approved plugin offers a profile yet. An owner installs a plugin and
    approves its origins on the <a href={resolve('/plugins')}>Plugins page</a>.
  </p>{/if}
{#if selected?.plugin}<section
    class="plugin-pin full"
    aria-label="Pinned plugin"
  >
    <dl>
      <div>
        <dt>Plugin</dt>
        <dd>{selected.plugin.name} {selected.plugin.version}</dd>
      </div>
      <div>
        <dt>Profile</dt>
        <dd>{selected.label} · <code>{selected.dialect}</code></dd>
      </div>
      <div class="digest">
        <dt>Digest</dt>
        <dd><code>{selected.plugin.digest}</code></dd>
      </div>
      {#if values.endpoint}<div>
          <dt>Address</dt>
          <dd><code>{values.endpoint}</code></dd>
        </div>{/if}
    </dl>
  </section>{/if}
{#if values.document && options.length}<fieldset
    class="plugin-options full"
    disabled={disabled || !values.document.fieldsAvailable}
  >
    <legend>Profile options</legend>
    <p class="plugin-note">
      The profile places these settings in the upstream address, headers and
      query parameters it declares, and hands them to the plugin. They are not
      secret.
    </p>
    <div class="form-grid">
      {#each options as option (option.name)}<NativeValueField
          draft={values.document}
          path={['options', 'plugin_options', option.name]}
          label={option.schema.title ?? option.name}
          id={`${idPrefix}-option-${option.name}`}
          schema={option.schema}
          required={option.required}
          problem={optionIssue(option.name)}
          onChange={changeOption}
        />{/each}
    </div>
  </fieldset>{/if}

<style>
  .full {
    grid-column: 1 / -1;
  }
  .plugin-note {
    margin: 0;
    color: var(--foreground-muted);
    font-size: var(--text-body-sm);
  }
  .plugin-note a {
    color: var(--foreground);
    text-decoration: underline;
    text-decoration-color: var(--border-strong);
    text-underline-offset: 4px;
  }
  .plugin-pin {
    padding: 0.85rem;
    border-radius: var(--radius-control);
    background: var(--surface-raised);
    font-size: var(--text-body-sm);
  }
  .plugin-pin dl {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(12rem, 1fr));
    gap: 0.75rem;
    margin: 0;
  }
  .plugin-pin dt {
    color: var(--foreground-muted);
    font-size: var(--text-caption);
  }
  .plugin-pin dd {
    margin: 0.2rem 0 0;
    min-width: 0;
  }
  .plugin-pin code {
    overflow-wrap: anywhere;
    font-size: var(--text-caption);
  }
  .plugin-pin .digest {
    grid-column: 1 / -1;
  }
  .plugin-options {
    display: grid;
    gap: 0.75rem;
    margin: 0;
    padding: 0;
    border: 0;
  }
  .plugin-options legend {
    margin-bottom: 0.5rem;
    font-weight: 500;
  }
</style>
