import { createQuery, useQueryClient } from '@tanstack/svelte-query';
import { useAction } from '#lib/editing/action.svelte.js';
import { usePrincipalLifetime } from '#lib/editing/mutationIntent.svelte.js';
import { isEtagMismatch } from '#lib/management/http.js';
import { useRole } from '#lib/session/useRole.svelte.js';
import { listProviderKinds } from './models';
import {
  createProviderDraft,
  requiresCredential,
  requiresGrant,
  type ProviderDraft
} from './providerEditor';
import { providerKeys } from './providerKeys';
import type { ProviderProbe } from './providers';
import { ProviderWizardActivationStage } from './providerWizardActivationStage';
import { ProviderWizardConnectionStage } from './providerWizardConnectionStage.svelte';
import { ProviderWizardModelStage } from './providerWizardModelStage.svelte';

export class ProviderWizardState {
  readonly stepLabels = [
    'Connection',
    'Models and capabilities',
    'Activation'
  ] as const;
  readonly access = useRole();
  readonly canManage = $derived(this.access.can('providers.manage'));
  readonly providerKinds = createQuery(() => ({
    queryKey: providerKeys.kinds(),
    queryFn: ({ signal }) => listProviderKinds(signal)
  }));
  draft = $state<ProviderDraft | null>(null);
  providerId = $state('');
  wizardStep = $state(1);
  probe = $state<ProviderProbe | null>(null);
  notice = $state('');
  wizardConflict = $state(false);
  readonly selectedSpec = $derived.by(() =>
    this.providerKinds.data?.find(
      (candidate) => candidate.kind === this.draft?.kind
    )
  );
  readonly credentialRequired = $derived(
    Boolean(
      this.draft &&
      this.selectedSpec &&
      requiresCredential(this.selectedSpec, this.draft.authMode)
    )
  );
  readonly grantRequired = $derived(
    Boolean(
      this.draft &&
      this.selectedSpec &&
      requiresGrant(this.selectedSpec, this.draft.authMode)
    )
  );

  readonly claimConflict = (cause: unknown) => {
    if (!this.wizardProvider || !isEtagMismatch(cause)) return false;
    this.wizardConflict = true;
    return true;
  };

  readonly action = useAction({
    conflict: this.claimConflict,
    begin: () => {
      this.notice = '';
    }
  });
  readonly models = new ProviderWizardModelStage(this);
  readonly wizardProvider = $derived(this.models.page.data?.provider ?? null);
  readonly connection = new ProviderWizardConnectionStage(this);
  readonly activation = new ProviderWizardActivationStage(
    this,
    useQueryClient()
  );

  get busy() {
    return this.action.key || this.connection.grant.busy;
  }

  get writable() {
    return this.canManage && !this.connection.grant.busy;
  }

  reloadWizard = async () => {
    if (!this.wizardProvider || !this.writable) return;
    await this.action.run(
      async (current) => {
        await this.models.refetch();
        if (!current()) return;
        this.models.reloadVersion += 1;
        this.wizardConflict = false;
      },
      undefined,
      { key: 'reload' }
    );
  };

  goBack = () => {
    if (this.busy || this.wizardStep <= 1) return;
    if (this.activation.unresolved) {
      this.action.error =
        'Activation may already have committed. Retry activation before changing this connection, or leave the wizard.';
      return;
    }
    this.wizardStep -= 1;
    const provider = this.wizardProvider;
    const spec = this.selectedSpec;
    const previous = this.draft;
    if (this.wizardStep !== 1 || !provider || !spec || !previous) return;
    // Discovery records upstream model facts in the stored configuration;
    // re-saving the pre-discovery document would erase them and reset
    // certification evidence, so the connection form edits the stored one.
    const draft = createProviderDraft(spec, provider.configuration);
    draft.name = provider.name;
    draft.projectId = previous.projectId;
    draft.model = previous.model;
    draft.credential = '';
    this.draft = draft;
  };

  startAnother = () => {
    this.action.retire();
    this.connection.reset();
    this.activation.reset();
    this.providerId = '';
    this.draft = null;
    this.wizardStep = 1;
    this.probe = null;
    this.connection.grant.abandon();
    this.models.reset();
    this.notice = '';
    this.wizardConflict = false;
  };

  constructor() {
    usePrincipalLifetime(() => {
      this.activation.reset();
      this.notice = '';
    });
    $effect(() => {
      const first = this.providerKinds.data?.[0];
      if (!this.draft && first) this.draft = createProviderDraft(first);
      if (this.draft && this.selectedSpec && !this.credentialRequired)
        this.draft.credential = '';
    });
  }
}
