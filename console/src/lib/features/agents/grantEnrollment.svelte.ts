import { useQueryClient } from '@tanstack/svelte-query';
import { usePrincipalLifetime } from '#lib/editing/mutationIntent.svelte.js';
import { errorMessage } from '#lib/management/http.js';
import { agentKeys } from './agentKeys';
import {
  authorizeMCPGrant,
  claimMCPGrant,
  completeMCPGrant,
  pollMCPGrant,
  startAgentGrant,
  startMCPGrant,
  type MCPGrantAuthorization
} from './api';

export type PersonalGrantPhase =
  | { readonly kind: 'idle' }
  | { readonly kind: 'handoff'; readonly id: string; readonly token: string }
  | {
      readonly kind: 'callback';
      readonly id: string;
      readonly code: string;
      readonly state: string;
    }
  | { readonly kind: 'claimed'; readonly id: string }
  | {
      readonly kind: 'authorization';
      readonly id: string;
      readonly authorization: MCPGrantAuthorization;
    };

export type PersonalGrantTarget = {
  readonly kind: 'server' | 'agent';
  readonly id: string;
  readonly keyId: string;
};

export class PersonalGrantEnrollment {
  private phase = $state<PersonalGrantPhase>({ kind: 'idle' });
  private invitationURL = $state('');
  private working = $state(false);
  private failure = $state('');
  private message = $state('');
  private epoch = 0;
  private readonly client = useQueryClient();
  private readonly principal = usePrincipalLifetime(() => this.reset());

  get view() {
    return {
      phase: this.phase,
      busy: this.working,
      error: this.failure,
      message: this.message,
      enrollmentID: 'id' in this.phase ? this.phase.id : '',
      handoffToken: this.phase.kind === 'handoff' ? this.phase.token : '',
      callbackCode: this.phase.kind === 'callback' ? this.phase.code : '',
      authorization:
        this.phase.kind === 'authorization'
          ? this.phase.authorization
          : undefined,
      enrollmentURL: this.invitationURL
    } as const;
  }

  reset = () => {
    this.epoch += 1;
    this.phase = { kind: 'idle' };
    this.invitationURL = '';
    this.working = false;
    this.failure = this.message = '';
  };

  clearInvitation = () => {
    this.invitationURL = '';
    this.failure = this.message = '';
  };

  consumeLocation = (location: URL): string | null => {
    const code = location.searchParams.get('code');
    const state = location.searchParams.get('state');
    if (code && state) {
      this.reset();
      this.phase = {
        kind: 'callback',
        id: state.split('.')[0] ?? '',
        code,
        state
      };
      return location.pathname;
    }
    const [id, token] = location.hash.slice(1).split('.');
    if (!id || !token) return null;
    this.reset();
    this.phase = { kind: 'handoff', id, token };
    return location.pathname + location.search;
  };

  private async run(action: (isCurrent: () => boolean) => Promise<void>) {
    if (this.working) return;
    const principal = this.principal();
    const epoch = this.epoch;
    const isCurrent = () => principal() && epoch === this.epoch;
    this.working = true;
    this.failure = this.message = '';
    try {
      await action(isCurrent);
    } catch (cause) {
      if (isCurrent()) this.failure = errorMessage(cause);
    } finally {
      if (isCurrent()) this.working = false;
    }
  }

  private async refresh(isCurrent: () => boolean) {
    try {
      await this.client.invalidateQueries(
        { queryKey: agentKeys.grants() },
        { throwOnError: true }
      );
    } catch (cause) {
      if (isCurrent())
        this.failure = `Write saved, but refreshing failed. ${errorMessage(cause)}`;
    }
  }

  begin = async (target: PersonalGrantTarget) => {
    if (this.working || !target.id || !target.keyId.trim()) return;
    this.clearInvitation();
    await this.run(async (isCurrent) => {
      const result =
        target.kind === 'server'
          ? await startMCPGrant(target.id, target.keyId.trim())
          : await startAgentGrant(target.id, target.keyId.trim());
      if (isCurrent()) this.invitationURL = result.enrollment_url;
    });
  };

  claim = async () => {
    const phase = this.phase;
    if (phase.kind !== 'handoff') return;
    await this.run(async (isCurrent) => {
      const claimed = await claimMCPGrant(phase.id, phase.token);
      if (!isCurrent()) return;
      this.phase = { kind: 'claimed', id: claimed.enrollment.id };
      this.message = 'Enrollment claimed. Authorization is pending';
      await this.refresh(isCurrent);
    });
  };

  authorize = async (flow: 'authorization_code' | 'device_code') => {
    const phase = this.phase;
    if (phase.kind !== 'claimed' && phase.kind !== 'authorization') return;
    await this.run(async (isCurrent) => {
      const authorization = await authorizeMCPGrant(phase.id, flow);
      if (isCurrent())
        this.phase = { kind: 'authorization', id: phase.id, authorization };
    });
  };

  private async complete(
    id: string,
    result: Promise<MCPGrantAuthorization>,
    isCurrent: () => boolean
  ) {
    const authorization = await result;
    if (!isCurrent()) return;
    const previous =
      this.phase.kind === 'authorization'
        ? this.phase.authorization
        : undefined;
    this.phase = {
      kind: 'authorization',
      id,
      authorization: { ...previous, ...authorization }
    };
    this.message =
      authorization.state === 'active'
        ? 'Personal authorization is active'
        : `Personal authorization is ${authorization.state}`;
    await this.refresh(isCurrent);
  }

  completeCallback = async () => {
    const phase = this.phase;
    if (phase.kind !== 'callback' || !phase.id) return;
    await this.run((isCurrent) =>
      this.complete(
        phase.id,
        completeMCPGrant(phase.id, phase.code, phase.state),
        isCurrent
      )
    );
  };

  poll = async () => {
    const phase = this.phase;
    if (phase.kind !== 'authorization') return;
    await this.run((isCurrent) =>
      this.complete(phase.id, pollMCPGrant(phase.id), isCurrent)
    );
  };
}
