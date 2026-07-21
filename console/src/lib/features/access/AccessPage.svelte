<script lang="ts">
  import { resolve } from '$app/paths';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { onDestroy } from 'svelte';
  import SecretDialog from '$lib/components/SecretDialog.svelte';
  import CursorPagination from '$lib/components/CursorPagination.svelte';
  import { ApiProblem } from '$lib/api/http';
  import {
    createInvitation,
    listInvitationPage,
    listSessionPage,
    listUserPage,
    revokeInvitation,
    revokeSession,
    updateUserActive,
    updateUserRole,
    type InvitationSecret,
    type User
  } from '$lib/api/management/access';
  import {
    beginOidcLink,
    getOidcConfiguration,
    putOidcConfiguration,
    type OidcConfiguration,
    type OidcConfigurationInput
  } from '$lib/api/management/oidc';
  import type { CursorPage } from '$lib/api/management/shared';

  type Tab = 'members' | 'invitations' | 'sessions' | 'oidc';
  const roles = ['owner', 'operator', 'developer', 'viewer'];
  const queryClient = useQueryClient();
  let tab = $state<Tab>('members');
  let selectedSessionUser = $state('');
  let userCursor = $state<string | undefined>();
  let userHistory = $state<Array<string | undefined>>([]);
  let invitationCursor = $state<string | undefined>();
  let invitationHistory = $state<Array<string | undefined>>([]);
  let sessionCursor = $state<string | undefined>();
  let sessionHistory = $state<Array<string | undefined>>([]);

  const users = createQuery(() => ({ queryKey: ['user-page', userCursor ?? 'first'], queryFn: () => listUserPage(userCursor) }));
  const invitations = createQuery(() => ({ queryKey: ['invitation-page', invitationCursor ?? 'first'], queryFn: () => listInvitationPage(invitationCursor), enabled: tab === 'invitations' }));
  const sessions = createQuery(() => ({ queryKey: ['session-page', selectedSessionUser, sessionCursor ?? 'first'], queryFn: () => listSessionPage(selectedSessionUser || undefined, sessionCursor), enabled: tab === 'sessions' }));
  const oidc = createQuery(() => ({ queryKey: ['oidc-configuration'], queryFn: getOidcConfiguration, enabled: tab === 'oidc', retry: false }));

  let busy = $state('');
  let errorMessage = $state('');
  let notice = $state('');
  let inviteEmail = $state('');
  let inviteRole = $state('developer');
  let invitationSecret = $state<InvitationSecret | null>(null);
  let copied = $state(false);
  let oidcInitialized = $state('pending');
  let oidcDiscoveryUrl = $state('');
  let oidcIssuer = $state('');
  let oidcClientId = $state('');
  let oidcClientSecret = $state('');
  let oidcEnabled = $state(false);
  let oidcScopes = $state('openid profile email');
  let oidcEmailClaim = $state('email');
  let oidcGroupsClaim = $state('groups');
  let oidcDefaultRole = $state('viewer');
  let oidcEmailMappings = $state('');
  let oidcGroupMappings = $state('');
  let previousSessionUser = $state('');

  $effect(() => {
    if (selectedSessionUser === previousSessionUser) return;
    previousSessionUser = selectedSessionUser;
    sessionCursor = undefined;
    sessionHistory = [];
  });

  $effect(() => {
    if (!oidc.isFetched || oidcInitialized !== 'pending') return;
    const value = oidc.data;
    oidcInitialized = value?.etag ?? 'new';
    if (!value) return;
    oidcDiscoveryUrl = value.discovery_url;
    oidcIssuer = value.issuer;
    oidcClientId = value.client_id;
    oidcEnabled = value.enabled;
    oidcScopes = value.scopes.join(' ');
    oidcEmailClaim = value.email_claim;
    oidcGroupsClaim = value.groups_claim;
    oidcDefaultRole = value.default_role ?? '';
    oidcEmailMappings = value.email_role_mappings.map((mapping) => `${mapping.claim_value}=${mapping.role}`).join('\n');
    oidcGroupMappings = value.group_role_mappings.map((mapping) => `${mapping.claim_value}=${mapping.role}`).join('\n');
  });

  onDestroy(() => {
    invitationSecret = null;
    oidcClientSecret = '';
  });

  function message(error: unknown) {
    return error instanceof ApiProblem
      ? error.problem.detail ?? error.problem.title
      : error instanceof Error ? error.message : 'The control API could not complete the request.';
  }

  async function run(label: string, action: () => Promise<void>) {
    busy = label; errorMessage = ''; notice = '';
    try { await action(); } catch (error) { errorMessage = message(error); } finally { busy = ''; }
  }

  async function changeRole(user: User, role: string) {
    if (role === user.role) return;
    await run(`role-${user.id}`, async () => {
      const updated = await updateUserRole(user, role);
      queryClient.setQueryData<CursorPage<User>>(['user-page', userCursor ?? 'first'], (current) => current ? {
        ...current,
        items: current.items.map((item) => item.id === updated.id ? updated : item)
      } : current);
      notice = `${updated.display_name} is now ${updated.role}. Existing sessions were revoked.`;
    });
  }

  async function changeActive(user: User) {
    const active = !user.active;
    if (!active && !confirm(`Deactivate ${user.display_name}? Every active session will be revoked. API keys are installation-scoped and will remain active; after deactivation, review the API-key inventory and explicitly rotate or revoke any keys attributed to this member.`)) return;
    await run(`active-${user.id}`, async () => {
      const updated = await updateUserActive(user, active);
      queryClient.setQueryData<CursorPage<User>>(['user-page', userCursor ?? 'first'], (current) => current ? {
        ...current,
        items: current.items.map((item) => item.id === updated.id ? updated : item)
      } : current);
      notice = active
        ? `${updated.display_name} can sign in again.`
        : `${updated.display_name} was deactivated and existing sessions were revoked. Next: review API Keys for keys attributed to this member; installation-scoped keys are not automatically revoked.`;
    });
  }

  async function invite(event: SubmitEvent) {
    event.preventDefault();
    if (!inviteEmail.trim() || !inviteEmail.includes('@')) { errorMessage = 'Enter a valid email address.'; return; }
    await run('invite', async () => {
      invitationSecret = await createInvitation(inviteEmail.trim(), inviteRole);
      inviteEmail = '';
      await invitations.refetch();
    });
  }

  async function removeInvitation(id: string, email: string) {
    if (!confirm(`Revoke the invitation for ${email}?`)) return;
    await run(`invitation-${id}`, async () => {
      await revokeInvitation(id);
      await invitations.refetch();
      notice = 'Invitation revoked.';
    });
  }

  async function removeSession(id: string, current: boolean) {
    if (!confirm(current ? 'Sign out this current session?' : 'Revoke this session?')) return;
    await run(`session-${id}`, async () => {
      await revokeSession(id);
      if (current) window.location.assign(resolve('/login'));
      else { await sessions.refetch(); notice = 'Session revoked.'; }
    });
  }

  function parseMappings(value: string) {
    return value.split('\n').map((line) => line.trim()).filter(Boolean).map((line) => {
      const separator = line.lastIndexOf('=');
      if (separator < 1) throw new Error(`Mapping “${line}” must use claim-value=role.`);
      const claim_value = line.slice(0, separator).trim();
      const role = line.slice(separator + 1).trim();
      if (!roles.includes(role)) throw new Error(`Mapping “${line}” has an invalid fixed role.`);
      return { claim_value, role };
    });
  }

  async function saveOidc(event: SubmitEvent) {
    event.preventDefault();
    if (!oidcDiscoveryUrl || !oidcIssuer || !oidcClientId) { errorMessage = 'Issuer, discovery URL, and client ID are required.'; return; }
    await run('oidc-save', async () => {
      const input: OidcConfigurationInput = {
        discovery_url: oidcDiscoveryUrl.trim(),
        issuer: oidcIssuer.trim(),
        client_id: oidcClientId.trim(),
        client_secret: oidcClientSecret || null,
        enabled: oidcEnabled,
        scopes: oidcScopes.split(/[ ,]+/).filter(Boolean),
        email_claim: oidcEmailClaim.trim() || 'email',
        groups_claim: oidcGroupsClaim.trim() || 'groups',
        default_role: oidcDefaultRole || null,
        email_role_mappings: parseMappings(oidcEmailMappings),
        group_role_mappings: parseMappings(oidcGroupMappings)
      };
      const updated = await putOidcConfiguration(input, oidc.data?.etag);
      oidcClientSecret = '';
      queryClient.setQueryData(['oidc-configuration'], updated);
      oidcInitialized = updated.etag;
      notice = updated.enabled ? 'OIDC configuration validated and enabled.' : 'OIDC configuration saved but disabled.';
    });
  }

  async function linkIdentity() {
    await run('oidc-link', async () => {
      const authorizationUrl = await beginOidcLink();
      window.location.assign(authorizationUrl);
    });
  }

  async function copyInvitation() {
    if (!invitationSecret) return;
    // Keep the one-time token in the URL fragment so it is never sent in an
    // HTTP request, Referer header, or static-console access log.
    const url = `${window.location.origin}/invitations/accept#token=${encodeURIComponent(invitationSecret.token)}`;
    await navigator.clipboard.writeText(url);
    copied = true;
  }

  function nextUserPage() {
    const next = users.data?.nextCursor;
    if (!next) return;
    userHistory = [...userHistory, userCursor];
    userCursor = next;
  }

  function previousUserPage() {
    userCursor = userHistory.at(-1);
    userHistory = userHistory.slice(0, -1);
  }

  function nextInvitationPage() {
    const next = invitations.data?.nextCursor;
    if (!next) return;
    invitationHistory = [...invitationHistory, invitationCursor];
    invitationCursor = next;
  }

  function previousInvitationPage() {
    invitationCursor = invitationHistory.at(-1);
    invitationHistory = invitationHistory.slice(0, -1);
  }

  function nextSessionPage() {
    const next = sessions.data?.nextCursor;
    if (!next) return;
    sessionHistory = [...sessionHistory, sessionCursor];
    sessionCursor = next;
  }

  function previousSessionPage() {
    sessionCursor = sessionHistory.at(-1);
    sessionHistory = sessionHistory.slice(0, -1);
  }
</script>

<svelte:head><title>Access · OpenLLMProxy</title></svelte:head>

{#if invitationSecret}
  <SecretDialog eyebrow="Invitation created" title="Copy the invitation link now." description={`The token is displayed once and expires at ${new Date(invitationSecret.invitation.expires_at).toLocaleString()}.`} onClose={() => { invitationSecret = null; copied = false; }}>
    {#snippet children(close)}
      <code class="invitation-token">{invitationSecret!.token}</code><div class="dialog-actions"><button class="button button-secondary" type="button" onclick={copyInvitation}>{copied ? 'Link copied' : 'Copy invitation link'}</button><button class="button button-primary" type="button" data-autofocus onclick={close}>I have shared it</button></div>
    {/snippet}
  </SecretDialog>
{/if}

<div class="page-header"><div><p class="eyebrow">Identity</p><h1 class="page-title">Access</h1><p class="page-description">Manage installation members, fixed roles, invitations, sessions, and the linked OIDC provider.</p></div><a class="button button-primary" href={resolve('/access')} onclick={(event) => { event.preventDefault(); tab = 'invitations'; }}>Invite member</a></div>

<nav class="tabs" aria-label="Access settings">{#each [['members', 'Members'], ['invitations', 'Invitations'], ['sessions', 'Sessions'], ['oidc', 'OIDC']] as item (item[0])}<button class:active={tab === item[0]} type="button" aria-current={tab === item[0] ? 'page' : undefined} onclick={() => tab = item[0] as Tab}>{item[1]}</button>{/each}</nav>

{#if errorMessage}<div class="inline-problem" role="alert">{errorMessage}</div>{/if}
{#if notice}<div class="success-banner" role="status">{notice}</div>{/if}

{#if tab === 'members'}
  <div class="role-guide" aria-label="Fixed role permissions">{#each [['owner', 'Full control, identity, and access'], ['operator', 'Gateway configuration and operations'], ['developer', 'Keys, playground, and request metadata'], ['viewer', 'Read-only monitoring']] as role (role[0])}<div><span class="badge accent">{role[0]}</span><small>{role[1]}</small></div>{/each}</div>
  {#if users.isPending}<div class="loading-state" role="status">Loading members…</div>{:else if users.isError}<div class="inline-problem" role="alert">{message(users.error)} <button class="button button-secondary" type="button" onclick={() => users.refetch()}>Retry</button></div>{:else}<div class="table-shell"><table class="data-table"><thead><tr><th>Member</th><th>Status</th><th>Fixed role</th><th>Joined</th><th><span class="sr-only">Actions</span></th></tr></thead><tbody>{#each users.data?.items ?? [] as user (user.id)}<tr><td><strong>{user.display_name}</strong><br /><span>{user.email}</span></td><td><span class:success={user.active} class:danger={!user.active} class="badge">{user.active ? 'active' : 'disabled'}</span></td><td><label><span class="sr-only">Role for {user.display_name}</span><select class="role-select" value={user.role} onchange={(event) => changeRole(user, event.currentTarget.value)} disabled={!user.active || busy === `role-${user.id}`}>{#each roles as role (role)}<option value={role}>{role}</option>{/each}</select></label></td><td>{new Date(user.created_at).toLocaleDateString()}</td><td><button class="button button-secondary" class:danger-button={user.active} type="button" onclick={() => changeActive(user)} disabled={Boolean(busy)}>{busy === `active-${user.id}` ? 'Saving…' : user.active ? 'Deactivate' : 'Reactivate'}</button></td></tr>{/each}</tbody></table></div><CursorPagination page={userHistory.length + 1} hasPrevious={userHistory.length > 0} hasNext={Boolean(users.data?.nextCursor)} onPrevious={previousUserPage} onNext={nextUserPage} label="Member pages" />{/if}
{:else if tab === 'invitations'}
  <section class="card invite-panel" aria-labelledby="invite-heading"><div><p class="eyebrow">New invitation</p><h2 id="invite-heading">Invite by email</h2><p>The acceptance token is shown once. No email service is required.</p></div><form onsubmit={invite}><label><span>Email address</span><input type="email" autocomplete="email" bind:value={inviteEmail} placeholder="person@example.com" /></label><label><span>Role</span><select bind:value={inviteRole}>{#each roles as role (role)}<option value={role}>{role}</option>{/each}</select></label><button class="button button-primary" type="submit" disabled={busy === 'invite'}>{busy === 'invite' ? 'Creating…' : 'Create invitation'}</button></form></section>
  {#if invitations.isPending}<div class="loading-state" role="status">Loading invitation history…</div>{:else if invitations.isError}<div class="inline-problem" role="alert">{message(invitations.error)}</div>{:else if !invitations.data?.items.length && invitationHistory.length === 0}<section class="card empty-state"><p>No invitations have been created.</p></section>{:else}<div class="table-shell"><table class="data-table"><thead><tr><th>Email</th><th>Role</th><th>Status</th><th>Expires</th><th><span class="sr-only">Actions</span></th></tr></thead><tbody>{#each invitations.data?.items ?? [] as invitation (invitation.id)}<tr><td>{invitation.email}</td><td><span class="badge">{invitation.role}</span></td><td><span class:success={invitation.status === 'accepted'} class:warning={invitation.status === 'pending'} class:danger={invitation.status === 'revoked'} class="badge">{invitation.status}</span></td><td>{new Date(invitation.expires_at).toLocaleString()}</td><td>{#if invitation.status === 'pending'}<button class="button button-secondary danger-button" type="button" onclick={() => removeInvitation(invitation.id, invitation.email)} disabled={Boolean(busy)}>Revoke</button>{/if}</td></tr>{/each}</tbody></table></div><CursorPagination page={invitationHistory.length + 1} hasPrevious={invitationHistory.length > 0} hasNext={Boolean(invitations.data?.nextCursor)} onPrevious={previousInvitationPage} onNext={nextInvitationPage} label="Invitation pages" />{/if}
{:else if tab === 'sessions'}
  <div class="toolbar"><div><p class="eyebrow">Active sessions</p><h2>Opaque server-side sessions</h2></div><label class="session-filter"><span>Member</span><select bind:value={selectedSessionUser}><option value="">My sessions</option>{#each users.data?.items ?? [] as user (user.id)}<option value={user.id}>{user.display_name}</option>{/each}</select></label></div>
  {#if sessions.isPending}<div class="loading-state" role="status">Loading sessions…</div>{:else if sessions.isError}<div class="inline-problem" role="alert">{message(sessions.error)}</div>{:else if !sessions.data?.items.length && sessionHistory.length === 0}<section class="card empty-state"><p>No active sessions in this view.</p></section>{:else}<div class="table-shell"><table class="data-table"><thead><tr><th>Session ID</th><th>Status</th><th>Created</th><th>Last seen</th><th>Expires</th><th><span class="sr-only">Actions</span></th></tr></thead><tbody>{#each sessions.data?.items ?? [] as session (session.id)}<tr><td><code>{session.id}</code></td><td><span class:accent={session.current} class="badge">{session.current ? 'current' : 'active'}</span></td><td>{new Date(session.created_at).toLocaleString()}</td><td>{new Date(session.last_seen_at).toLocaleString()}</td><td>{new Date(session.expires_at).toLocaleString()}</td><td><button class="button button-secondary danger-button" type="button" onclick={() => removeSession(session.id, session.current)} disabled={Boolean(busy)}>{session.current ? 'Sign out' : 'Revoke'}</button></td></tr>{/each}</tbody></table></div><CursorPagination page={sessionHistory.length + 1} hasPrevious={sessionHistory.length > 0} hasNext={Boolean(sessions.data?.nextCursor)} onPrevious={previousSessionPage} onNext={nextSessionPage} label="Session pages" />{/if}
{:else}
  {#if oidc.isPending}<div class="loading-state" role="status">Loading OIDC configuration…</div>{:else if oidc.isError}<div class="inline-problem" role="alert">{message(oidc.error)} <button class="button button-secondary" type="button" onclick={() => oidc.refetch()}>Retry</button></div>{:else}
    <form class="oidc-grid" onsubmit={saveOidc}>
      <section class="card oidc-form" aria-labelledby="oidc-heading"><div class="section-heading"><div><p class="eyebrow">Single identity provider</p><h2 id="oidc-heading">OIDC Authorization Code + PKCE</h2></div><label class="enabled"><input type="checkbox" bind:checked={oidcEnabled} /> Enabled</label></div><p class="muted">Discovery metadata is validated server-side and must match the issuer configured with your identity provider. Every flow uses PKCE, state, and nonce; identities require explicit linking.</p><div class="form-grid"><div class="form-field full"><label for="oidc-issuer">Expected issuer</label><input id="oidc-issuer" type="url" bind:value={oidcIssuer} placeholder="https://id.example.com" required /></div><div class="form-field full"><label for="discovery-url">Discovery URL</label><input id="discovery-url" type="url" bind:value={oidcDiscoveryUrl} placeholder="https://id.example.com/.well-known/openid-configuration" required /></div><div class="form-field"><label for="client-id">Client ID</label><input id="client-id" autocomplete="off" bind:value={oidcClientId} required /></div><div class="form-field"><label for="client-secret">Client secret</label><input id="client-secret" type="password" autocomplete="new-password" bind:value={oidcClientSecret} placeholder={oidc.data?.has_client_secret ? 'Leave blank to keep current secret' : 'Write-only secret'} /></div><div class="form-field full"><label for="oidc-scopes">Scopes</label><input id="oidc-scopes" bind:value={oidcScopes} /></div><div class="form-field"><label for="email-claim">Email claim</label><input id="email-claim" bind:value={oidcEmailClaim} /></div><div class="form-field"><label for="groups-claim">Groups claim</label><input id="groups-claim" bind:value={oidcGroupsClaim} /></div><div class="form-field"><label for="default-role">Default role</label><select id="default-role" bind:value={oidcDefaultRole}><option value="">No default (mapping required)</option>{#each roles as role (role)}<option value={role}>{role}</option>{/each}</select></div></div></section>
      <section class="card mapping-form" aria-labelledby="mapping-heading"><p class="eyebrow">Authorization mapping</p><h2 id="mapping-heading">Claims to fixed roles</h2><p class="muted">One mapping per line in <code>claim-value=role</code> form. Email mappings take precedence over group mappings and the default.</p><div class="form-field"><label for="email-mappings">Email mappings</label><textarea id="email-mappings" bind:value={oidcEmailMappings} placeholder="owner@example.com=owner"></textarea></div><div class="form-field"><label for="group-mappings">Group mappings</label><textarea id="group-mappings" bind:value={oidcGroupMappings} placeholder="platform-team=operator"></textarea></div><div class="oidc-actions"><button class="button button-secondary" type="button" onclick={linkIdentity} disabled={!oidc.data?.enabled || Boolean(busy)}>{busy === 'oidc-link' ? 'Redirecting…' : 'Link my identity'}</button><button class="button button-primary" type="submit" disabled={Boolean(busy)}>{busy === 'oidc-save' ? 'Validating…' : 'Save and validate'}</button></div></section>
    </form>
  {/if}
{/if}

<style>
  .tabs { display: flex; gap: .25rem; margin: 1.5rem 0; overflow-x: auto; border-bottom: 1px solid var(--border); }
  .tabs button { min-height: 2.75rem; padding: .65rem .85rem; border: 0; border-bottom: 2px solid transparent; background: transparent; color: var(--foreground-muted); white-space: nowrap; }
  .tabs button.active { border-color: var(--accent); color: var(--accent-strong); font-weight: 750; }
  .success-banner { margin: 1rem 0; padding: .85rem 1rem; border: 1px solid color-mix(in srgb, var(--success) 45%, var(--border)); border-radius: .375rem; background: var(--success-soft); color: var(--success); }
  h2 { margin: 0; font-size: 1.15rem; letter-spacing: -.025em; }
  .role-guide { display: grid; grid-template-columns: repeat(4, 1fr); gap: .65rem; margin-bottom: 1rem; }
  .role-guide div { display: grid; align-content: start; gap: .45rem; min-height: 5.5rem; padding: .8rem; border: 1px solid var(--border); border-radius: .375rem; background: var(--surface); }
  .role-guide .badge { justify-self: start; } .role-guide small, td span, .muted { color: var(--foreground-muted); }
  .role-select, .session-filter select, .invite-panel input, .invite-panel select { min-height: 2.5rem; padding: .5rem .65rem; border: 1px solid var(--border-strong); border-radius: .375rem; background: var(--surface); color: var(--foreground); }
  .invite-panel { display: flex; align-items: end; justify-content: space-between; gap: 2rem; margin-bottom: 1rem; padding: 1.25rem; }
  .invite-panel p { margin: .4rem 0 0; color: var(--foreground-muted); }
  .invite-panel form { display: flex; align-items: end; gap: .65rem; }
  .invite-panel label, .session-filter { display: grid; gap: .3rem; color: var(--foreground-muted); font-size: .72rem; font-weight: 700; }
  .danger-button { color: var(--danger); }
  code { font: .72rem 'JetBrains Mono Variable', monospace; }
  .toolbar h2 { margin: 0; }
  .oidc-grid { display: grid; grid-template-columns: minmax(0, 1.2fr) minmax(22rem, .8fr); gap: 1rem; }
  .oidc-form, .mapping-form { padding: clamp(1.15rem, 3vw, 1.5rem); }
  .section-heading { display: flex; align-items: start; justify-content: space-between; gap: 1rem; }
  .enabled { display: flex; min-height: 2.75rem; align-items: center; gap: .45rem; font-weight: 700; }
  .mapping-form { display: grid; align-content: start; gap: 1rem; }
  .mapping-form h2, .mapping-form p { margin: 0; }
  .oidc-actions, .dialog-actions { display: flex; justify-content: flex-end; gap: .65rem; }
  .invitation-token { display: block; overflow-x: auto; padding: .85rem; border: 1px solid var(--border); border-radius: .375rem; background: var(--surface-subtle); }
  @media (max-width: 64rem) { .role-guide { grid-template-columns: repeat(2, 1fr); } .oidc-grid { grid-template-columns: 1fr; } .invite-panel { display: grid; } }
  @media (max-width: 42rem) { .role-guide { grid-template-columns: 1fr; } .invite-panel form { display: grid; } .dialog-actions { display: grid; } }
</style>
