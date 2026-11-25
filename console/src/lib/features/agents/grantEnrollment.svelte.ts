import { useQueryClient } from '@tanstack/svelte-query';
import { useAction, type ActionFeedback } from '#lib/editing/action.svelte.js';
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

const feedback: ActionFeedback = {
  failure: (cause, stage) =>
    stage === 'accept'
      ? `Write saved, but refreshing failed. ${errorMessage(cause)}`
      : errorMessage(cause)
};

export class PersonalGrantEnrollment {
  private phase = $state<PersonalGrantPhase>({ kind: 'idle' });
  private invitationURL = $state('');
  private message = $state('');
  private readonly client = useQueryClient();
  private readonly action = useAction({
    begin: () => {
      this.message = '';
    }
  });

  constructor() {
    usePrincipalLifetime(this.reset);
  }

  get view() {
    return {
      phase: this.phase,
      busy: this.action.busy,
      error: this.action.error,
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
    this.action.retire();
    this.phase = { kind: 'idle' };
    this.invitationURL = this.message = '';
  };

  clearInvitation = () => {
    this.action.reset();
    this.invitationURL = this.message = '';
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

  private refresh = () =>
    this.client.invalidateQueries(
      { queryKey: agentKeys.grants() },
      { throwOnError: true }
    );

  begin = async (target: PersonalGrantTarget) => {
    if (this.action.busy || !target.id || !target.keyId.trim()) return;
    this.clearInvitation();
    await this.action.run(
      () =>
        target.kind === 'server'
          ? startMCPGrant(target.id, target.keyId.trim())
          : startAgentGrant(target.id, target.keyId.trim()),
      (result) => {
        this.invitationURL = result.enrollment_url;
      },
      feedback
    );
  };

  claim = async () => {
    const phase = this.phase;
    if (phase.kind !== 'handoff') return;
    await this.action.run(
      () => claimMCPGrant(phase.id, phase.token),
      async (claimed) => {
        this.phase = { kind: 'claimed', id: claimed.enrollment.id };
        this.message = 'Enrollment claimed. Authorization is pending';
        await this.refresh();
      },
      feedback
    );
  };

  authorize = async (flow: 'authorization_code' | 'device_code') => {
    const phase = this.phase;
    if (phase.kind !== 'claimed' && phase.kind !== 'authorization') return;
    await this.action.run(
      () => authorizeMCPGrant(phase.id, flow),
      (authorization) => {
        this.phase = { kind: 'authorization', id: phase.id, authorization };
      },
      feedback
    );
  };

  private complete = async (
    id: string,
    authorization: MCPGrantAuthorization
  ) => {
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
    await this.refresh();
  };

  completeCallback = async () => {
    const phase = this.phase;
    if (phase.kind !== 'callback' || !phase.id) return;
    await this.action.run(
      () => completeMCPGrant(phase.id, phase.code, phase.state),
      (authorization) => this.complete(phase.id, authorization),
      feedback
    );
  };

  poll = async () => {
    const phase = this.phase;
    if (phase.kind !== 'authorization') return;
    await this.action.run(
      () => pollMCPGrant(phase.id),
      (authorization) => this.complete(phase.id, authorization),
      feedback
    );
  };
}
