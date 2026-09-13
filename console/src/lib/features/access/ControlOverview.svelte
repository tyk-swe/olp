<script lang="ts">
  import { resolve } from '$app/paths';
  import { useRole } from './session/useRole.svelte';
  const access = useRole();
</script>

<div class="page-header">
  <div>
    <p class="eyebrow">Installation</p>
    <h1 class="page-title">Access and control</h1>
    <p class="page-description">
      Manage sign-in, members, client keys, and installation settings.
    </p>
  </div>
</div>
<div class="controls">
  {#if access.can('users.read')}
    <a class="card" href={resolve('/access')}
      ><h2>Members and sign-in</h2>
      <p>
        Invite members, assign roles, manage sessions, and configure your
        identity provider.
      </p></a
    >
  {/if}
  <a class="card" href={resolve('/api-keys')}
    ><h2>API keys</h2>
    <p>
      Create scoped client credentials, save policies, and rotate or revoke
      secrets.
    </p></a
  >
  <a class="card" href={resolve('/settings/profile')}
    ><h2>Your profile</h2>
    <p>
      Update your details, password, linked identities, and active sessions.
    </p></a
  >
  <a class="card" href={resolve('/settings')}
    ><h2>Installation settings</h2>
    <p>
      Manage local sign-in and review saved retention and limit policies.
    </p></a
  >
  <a class="card" href={resolve('/audit')}
    ><h2>Audit trail</h2>
    <p>
      Review account and configuration changes with their recorded actor and
      outcome.
    </p></a
  >
</div>
<p class="muted">
  Gateway requests, limit enforcement, and automatic retention are not available
  on this installation yet.
</p>

<style>
  .controls {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(min(100%, 20rem), 1fr));
    gap: 1rem;
    margin: 1.5rem 0;
  }
  .controls a {
    padding: 1.5rem;
    color: var(--foreground);
    text-decoration: none;
  }
  .controls a:hover {
    border-color: var(--border-strong);
  }
  h2 {
    margin: 0 0 0.75rem;
    font-size: 1.1rem;
    font-weight: 500;
  }
  p {
    color: var(--foreground-muted);
  }
</style>
