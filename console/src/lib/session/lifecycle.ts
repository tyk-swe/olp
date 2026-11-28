import type { QueryClient } from '@tanstack/svelte-query';
import {
  clearCsrfToken,
  getCsrfToken,
  setCsrfToken
} from '#lib/session/csrf.js';
import { QueryPartition } from '#lib/session/queryPartition.js';
import { AuthenticatedRequests } from '#lib/session/authenticatedRequests.js';
import { LatestRun } from '#lib/session/latestRun.js';
import {
  redirectToSignIn,
  type SignInBoundary
} from '#lib/session/signInRedirect.js';
import { SnapshotStore } from '#lib/session/snapshotStore.js';
import { TabChannel } from '#lib/session/tabChannel.js';
import { abortError, errorMessage } from '#lib/management/http.js';
import {
  sessionIsFresh,
  unauthorizedError
} from '#lib/session/sessionFreshness.js';
import type {
  AuthenticatedSession,
  AuthenticatedUser,
  AuthenticationSnapshot,
  PrincipalAbsentSnapshot
} from '#lib/session/state.js';

type Boundary = SignInBoundary & {
  loadSession(signal: AbortSignal): Promise<AuthenticatedSession>;
  loginDestination(): string;
};

type ValidateOptions = { passive?: boolean };

type PrincipalExitRequest = (signal: AbortSignal) => Promise<void>;
type AuthenticationRequest = (
  signal: AbortSignal
) => Promise<AuthenticatedSession>;

const SESSION_LOAD_FAILED = 'The current session could not be loaded.';

export class AuthenticationLifecycle {
  private queries = new QueryPartition();
  private readonly tabs = new TabChannel();
  private readonly snapshots = new SnapshotStore();
  private readonly requests = new AuthenticatedRequests({
    isAuthenticated: () => this.snapshot().phase === 'authenticated',
    ensureFresh: () => this.ensureFreshSession(),
    adoptRotatedToken: (token) => this.adoptRotatedToken(token),
    replaceRejectedToken: () => this.replaceRejectedToken(),
    confirmUnauthorized: () => this.confirmUnauthorized()
  });
  private hasEstablishedSession = false;

  private boundary: Boundary | null = null;
  private boundaryGeneration = 0;
  private readonly validation = new LatestRun<AuthenticatedSession | null>();
  private readonly redirect = new LatestRun<void>();
  private authenticationController: AbortController | null = null;
  private principalExitController: AbortController | null = null;
  private authenticationGeneration = 0;
  private unauthorizedHandled = false;

  connectTabs(): () => void {
    return this.tabs.connect({
      changed: () => void this.refreshAfterTabChange(),
      rotated: () => this.revalidateRotatedToken()
    });
  }

  private revalidateRotatedToken(): void {
    this.supersedeSessionResponses();
    clearCsrfToken();
    const snapshot = this.snapshot();
    if (snapshot.phase === 'authenticated') {
      this.snapshots.replace({ ...snapshot, lastValidatedAt: 0 });
      void this.validateSession({ passive: true });
    } else if (snapshot.phase === 'checking') {
      void this.validateSession();
    }
  }

  private async refreshAfterTabChange(): Promise<void> {
    this.abortAuthenticationWork();
    this.gateProtectedContent('checking');
    this.queries.rotateAnonymous();
    clearCsrfToken();
    await this.queries.cancelAndClear();
    await this.validateSession();
  }

  attachQueryClient(client: QueryClient): () => void {
    return this.queries.attach(client);
  }

  registerBoundary(boundary: Boundary): () => void {
    this.abortBoundaryWork();
    this.boundary = boundary;
    const generation = ++this.boundaryGeneration;
    return () => {
      if (generation !== this.boundaryGeneration) return;
      this.boundary = null;
      this.abortBoundaryWork();
    };
  }

  subscribe(listener: (snapshot: AuthenticationSnapshot) => void): () => void {
    return this.snapshots.subscribe(listener);
  }

  snapshot(): AuthenticationSnapshot {
    return this.snapshots.current();
  }

  markProtectedBoundaryChecking(): void {
    this.gateProtectedContent('checking');
  }

  queryKeyHash(key: readonly unknown[]): string {
    return this.queries.keyHash(key);
  }

  async authenticate(
    request: AuthenticationRequest
  ): Promise<AuthenticatedSession> {
    const generation = ++this.authenticationGeneration;
    this.authenticationController?.abort();
    this.validation.abort();
    this.redirect.interrupt();
    this.principalExitController?.abort();
    const controller = new AbortController();
    this.authenticationController = controller;
    const assertCurrent = () => {
      if (
        generation !== this.authenticationGeneration ||
        controller.signal.aborted
      ) {
        throw new DOMException('Authentication was superseded.', 'AbortError');
      }
    };
    await this.withdrawPrincipal('transitioning');
    assertCurrent();
    clearCsrfToken();
    this.queries.rotateAnonymous();
    const session = await request(controller.signal);
    assertCurrent();
    this.establishSession(session);
    this.tabs.announce('changed');
    return session;
  }

  establishSession(session: AuthenticatedSession): void {
    this.hasEstablishedSession = true;
    const partition = this.principalPartition(session.user);
    if (partition !== this.queries.current()) this.queries.use(partition);
    if (session.csrf_token) setCsrfToken(session.csrf_token);
    else clearCsrfToken();
    this.unauthorizedHandled = false;
    this.snapshots.apply({
      type: 'authenticated',
      session,
      validatedAt: Date.now()
    });
  }

  async revalidateWhenStale(): Promise<void> {
    if (this.validationIsCurrent()) return;
    await this.validateSession({ passive: true });
  }

  private validationIsCurrent(): boolean {
    const snapshot = this.snapshot();
    return (
      snapshot.phase === 'authenticated' &&
      !snapshot.error &&
      sessionIsFresh(snapshot.lastValidatedAt, Boolean(getCsrfToken()))
    );
  }

  async validateSession(
    options: ValidateOptions = {}
  ): Promise<AuthenticatedSession | null> {
    const boundary = this.boundary;
    if (!boundary) return null;
    const pending = this.validation.pending();
    if (
      options.passive &&
      (pending || this.snapshot().phase !== 'authenticated')
    ) {
      return pending;
    }
    return this.validation.start((signal) =>
      this.runValidation(boundary, signal)
    );
  }

  private async runValidation(
    boundary: Boundary,
    signal: AbortSignal
  ): Promise<AuthenticatedSession | null> {
    this.unauthorizedHandled = false;
    const snapshot = this.snapshot();
    if (snapshot.phase !== 'authenticated')
      this.gateProtectedContent('checking');
    else if (snapshot.error) this.snapshots.replace({ ...snapshot, error: '' });

    try {
      const session = await boundary.loadSession(signal);
      if (signal.aborted) return null;
      if (this.principalPartition(session.user) !== this.queries.current()) {
        await this.withdrawPrincipal('checking');
        if (signal.aborted) return null;
      }
      // Full-page OIDC callbacks bypass response middleware. Announce only
      // the first verified session in this document; sibling refreshes must
      // never echo the notification.
      const announceInitialSession = !this.hasEstablishedSession;
      this.establishSession(session);
      if (announceInitialSession) this.tabs.announce('rotated');
      return session;
    } catch (error) {
      if (signal.aborted || abortError(error)) return null;
      await this.recoverFailedValidation(
        error,
        snapshot.phase === 'authenticated',
        signal
      );
      return null;
    }
  }

  private async recoverFailedValidation(
    error: unknown,
    wasAuthenticated: boolean,
    signal: AbortSignal
  ): Promise<void> {
    if (unauthorizedError(error)) return this.transitionToAnonymous();
    if (this.unauthorizedHandled && this.snapshot().phase !== 'authenticated')
      return;
    const message = errorMessage(error, SESSION_LOAD_FAILED);
    if (wasAuthenticated) {
      this.snapshots.apply({ type: 'validation-error', error: message });
      return;
    }
    await this.withdrawPrincipal('unavailable', message);
    if (signal.aborted) return;
    clearCsrfToken();
    this.queries.rotateAnonymous();
  }

  async ensureFreshSession(): Promise<void> {
    const snapshot = this.snapshot();
    if (!snapshot.user || snapshot.phase !== 'authenticated') {
      throw new DOMException(
        'No authenticated principal is active.',
        'AbortError'
      );
    }
    const startingPartition = this.queries.current();
    const pending = this.validation.pending();
    if (!pending && this.validationIsCurrent()) return;
    const session = await (pending ?? this.validateSession());
    if (!session)
      throw new Error(
        this.snapshot().error ||
          'Session verification was interrupted. Retry before making changes.'
      );
    if (startingPartition !== this.queries.current()) {
      throw new DOMException(
        'The authenticated principal changed while the request was being prepared.',
        'AbortError'
      );
    }
    if (!getCsrfToken()) {
      throw new DOMException(
        'This session cannot make changes until you sign in again.',
        'InvalidStateError'
      );
    }
  }

  prepareRequest(request: Request): Promise<Request> {
    return this.requests.prepare(request);
  }

  handleResponse(request: Request, response: Response): Promise<void> {
    return this.requests.handleResponse(request, response);
  }

  private supersedeSessionResponses(): void {
    this.validation.abort();
    this.requests.supersede();
  }

  private adoptRotatedToken(token: string): void {
    // Earlier responses, including validation and rotation replies, no
    // longer describe the cookie installed by this response. Do not abort
    // its body while the transport is still reading it.
    this.supersedeSessionResponses();
    setCsrfToken(token);
    const snapshot = this.snapshot();
    if (snapshot.phase === 'authenticated') {
      this.snapshots.replace({
        ...snapshot,
        lastValidatedAt: Date.now(),
        error: ''
      });
    }
    this.tabs.announce('rotated');
  }

  private async replaceRejectedToken(): Promise<string> {
    this.supersedeSessionResponses();
    clearCsrfToken();
    await this.validateSession();
    return this.snapshot().error;
  }

  private async confirmUnauthorized(): Promise<void> {
    // A 401 can belong to a request sent with a cookie that another tab has
    // since replaced. Validate the browser's current cookie before deciding
    // that this tab has become anonymous.
    if (this.boundary) {
      await this.validateSession({ passive: true });
      return;
    }
    await this.transitionToAnonymous();
  }

  async signOut(
    request: PrincipalExitRequest,
    destination: string
  ): Promise<void> {
    if (!(await this.runPrincipalExit(request))) return;
    const boundary = this.boundary;
    if (boundary) await boundary.navigate(destination);
  }

  async endCurrentSession(request: PrincipalExitRequest): Promise<void> {
    if (!(await this.runPrincipalExit(request))) return;
    const boundary = this.boundary;
    if (boundary) await boundary.navigate(boundary.loginDestination());
  }

  abortAuthenticationWork(): void {
    this.authenticationController?.abort();
    this.principalExitController?.abort();
    this.abortBoundaryWork();
    this.requests.abort();
  }

  reset(): void {
    this.abortAuthenticationWork();
    this.tabs.close();
    this.hasEstablishedSession = false;
    this.boundary = null;
    this.boundaryGeneration = 0;
    this.authenticationController = null;
    this.principalExitController = null;
    this.requests.reset();
    this.authenticationGeneration = 0;
    this.unauthorizedHandled = false;
    this.queries = new QueryPartition();
    clearCsrfToken();
    this.snapshots.reset();
  }

  private async runPrincipalExit(
    request: PrincipalExitRequest
  ): Promise<boolean> {
    this.principalExitController?.abort();
    this.validation.abort();
    const controller = new AbortController();
    this.principalExitController = controller;
    await this.withdrawPrincipal('transitioning');
    this.queries.rotateAnonymous();
    try {
      await request(controller.signal);
      if (controller.signal.aborted) return false;
      clearCsrfToken();
      this.snapshots.apply({ type: 'anonymous' });
      this.tabs.announce('changed');
      return true;
    } catch (error) {
      if (controller.signal.aborted || abortError(error)) return false;
      await this.validateSession();
      if (
        controller.signal.aborted ||
        (this.unauthorizedHandled && !this.snapshot().user)
      )
        return false;
      this.snapshots.apply({
        type: 'principal-exit-error',
        error: errorMessage(
          error,
          'The sign-out request could not be completed.'
        )
      });
      throw error;
    } finally {
      if (this.principalExitController === controller)
        this.principalExitController = null;
    }
  }

  private transitionToAnonymous(): Promise<void> {
    const pending = this.redirect.pending();
    if (pending) return pending;
    const sessionExpired = this.snapshot().phase === 'authenticated';
    if (sessionExpired) this.tabs.announce('changed');
    this.unauthorizedHandled = true;
    this.gateProtectedContent('transitioning');
    this.authenticationController?.abort();
    this.validation.abort();
    this.principalExitController?.abort();
    this.requests.abort();
    clearCsrfToken();
    this.queries.rotateAnonymous();

    const boundary = this.boundary;
    return this.redirect.start((signal) =>
      redirectToSignIn(
        boundary,
        sessionExpired,
        this.queries.cancelAndClear(),
        (action) => this.snapshots.apply(action),
        signal
      )
    );
  }

  private abortBoundaryWork(): void {
    this.validation.abort();
    this.redirect.abort();
  }

  // Cached queries belong to one principal's exact authority: a changed role,
  // access scope, or operation set starts a fresh partition.
  principalPartition(user: AuthenticatedUser): string {
    return `principal:${user.id}:${user.role}:${user.access_scope}:${[...user.operations].sort().join(',')}`;
  }

  onPrincipalChange(listener: () => void): () => void {
    const partition = (snapshot: AuthenticationSnapshot) =>
      snapshot.user ? this.principalPartition(snapshot.user) : '';
    let current = partition(this.snapshot());
    return this.subscribe((snapshot) => {
      const next = partition(snapshot);
      if (next !== current) {
        current = next;
        listener();
      }
    });
  }

  private withdrawPrincipal(
    phase: PrincipalAbsentSnapshot['phase'],
    error = ''
  ): Promise<void> {
    this.gateProtectedContent(phase, error);
    this.requests.abort();
    return this.queries.cancelAndClear();
  }

  private gateProtectedContent(
    phase: PrincipalAbsentSnapshot['phase'],
    error = ''
  ): void {
    this.snapshots.apply({ type: 'gate', phase, error });
  }
}

export const authLifecycle = new AuthenticationLifecycle();
