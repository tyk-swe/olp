<script lang="ts">
  import PageTitle from '#lib/shell/branding/PageTitle.svelte';
  import OidcConfigurationPanel from '#lib/features/access/oidc/OidcConfigurationPanel.svelte';
  import SAMLConfigurationPanel from '#lib/features/access/saml/SAMLConfigurationPanel.svelte';
</script>

<PageTitle title="Single sign-on" />

<p class="lede">
  Let people sign in with your company identity provider. Set up OpenID Connect
  or SAML, whichever your provider supports.
</p>
<div class="stack">
  <OidcConfigurationPanel />
  <SAMLConfigurationPanel />
</div>

<style>
  .lede {
    max-width: 44rem;
    margin: 0 0 1.5rem;
    color: var(--foreground-muted);
  }
  .stack {
    display: grid;
    gap: 1.5rem;
  }
</style>
