import type { QueryClient } from '@tanstack/svelte-query';
import { clearCsrfToken, getCsrfToken, setCsrfToken } from '$lib/api/session';
import { QueryPartition } from './queryPartition';
import {
  isAuthenticationEndpoint,
  isCurrentSessionDeletion,
  isMutationRequest,
  isSessionValidationEndpoint
} from './requestPolicy';
import {
  abortError,
  sessionIsFresh,
  unauthorizedError
} from './sessionFreshness';
import {
  anonymousAuthenticationSnapshot,
  reduceAuthentication,
  type AuthenticatedSession,
  type AuthenticatedUser,
  type AuthenticationAction,
  type AuthenticationSnapshot,
  type PrincipalAbsentSnapshot
} from './state';

type Boundary = {
  loadSession(signal: AbortSignal): Promise<AuthenticatedSession>;
  unauthenticatedDestination(
    signal: AbortSignal,
    sessionExpired: boolean
  ): Promise<string>;
  loginDestination(): string;
  navigate(destination: string): Promise<void>;
};

type ValidateOptions = { passive?: boolean };

type PrincipalExitRequest = (signal: AbortSignal) => Promise<void>;
type AuthenticationRequest = (
  signal: AbortSignal
) => Promise<AuthenticatedSession>;

export class AuthenticationLifecycle {
  private queries = new QueryPartition();
  private boundary: Boundary | null = null;
  private boundaryGeneration = 0;
  private listeners = new Set<(snapshot: AuthenticationSnapshot) => void>();
  private snapshotValue: AuthenticationSnapshot =
    anonymousAuthenticationSnapshot();
  private sessionController: AbortController | null = null;
  private transitionController: AbortController | null = null;
  private authenticationController: AbortController | null = null;
  private principalExitController: AbortController | null = null;
  private authenticatedRequestController = new AbortController();
  private authenticatedRequestGeneration = 0;
  private requestGenerations = new WeakMap<Request, number>();
  private validationGeneration = 0;
  private authenticationGeneration = 0;
  private activeValidation: Promise<AuthenticatedSession | null> | null = null;
  private unauthorizedTransition: Promise<void> | null = null;
  private unauthorizedHandled = false;

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
    this.listeners.add(listener);
    listener(this.snapshotValue);
    return () => this.listeners.delete(listener);
  }

  snapshot(): AuthenticationSnapshot {
    return this.snapshotValue;
  }

  markProtectedBoundaryChecking(): void {
    this.apply({ type: 'gate', phase: 'checking' });
  }

  queryKeyHash(key: readonly unknown[]): string {
    return this.queries.keyHash(key);
  }

  async authenticate(
    request: AuthenticationRequest
  ): Promise<AuthenticatedSession> {
    const generation = ++this.authenticationGeneration;
    this.authenticationController?.abort();
    this.abortSessionValidation();
    this.transitionController?.abort();
    this.principalExitController?.abort();
    const controller = new AbortController();
    this.authenticationController = controller;
    this.gateProtectedContent('transitioning');
    this.rotateAuthenticatedRequests();
    await this.queries.cancelAndClear();
    if (
      generation !== this.authenticationGeneration ||
      controller.signal.aborted
    ) {
      throw new DOMException('Authentication was superseded.', 'AbortError');
    }
    clearCsrfToken();
    this.queries.rotateAnonymous();
    const session = await request(controller.signal);
    if (
      generation !== this.authenticationGeneration ||
      controller.signal.aborted
    ) {
      throw new DOMException('Authentication was superseded.', 'AbortError');
    }
    this.establishSession(session);
    return session;
  }

  establishSession(session: AuthenticatedSession): void {
    const partition = this.principalPartition(session.user);
    if (partition !== this.queries.current()) this.queries.use(partition);
    if (session.csrf_token) setCsrfToken(session.csrf_token);
    else clearCsrfToken();
    this.unauthorizedHandled = false;
    this.apply({ type: 'authenticated', session, validatedAt: Date.now() });
  }

  async validateSession(
    options: ValidateOptions = {}
  ): Promise<AuthenticatedSession | null> {
    const boundary = this.boundary;
    if (!boundary) return null;
    if (options.passive && this.activeValidation) return this.activeValidation;
    if (options.passive && this.snapshotValue.phase !== 'authenticated') {
      return this.activeValidation;
    }

    this.abortSessionValidation();
    const controller = new AbortController();
    this.sessionController = controller;
    const generation = this.validationGeneration;
    this.unauthorizedHandled = false;
    const authenticatedSnapshot =
      this.snapshotValue.phase === 'authenticated' ? this.snapshotValue : null;
    if (authenticatedSnapshot) {
      this.setSnapshot({ ...authenticatedSnapshot, error: '' });
    } else {
      this.apply({ type: 'gate', phase: 'checking' });
    }

    const validation = (async (): Promise<AuthenticatedSession | null> => {
      try {
        const session = await boundary.loadSession(controller.signal);
        if (
          controller.signal.aborted ||
          generation !== this.validationGeneration
        )
          return null;
        const nextPartition = this.principalPartition(session.user);
        if (nextPartition !== this.queries.current()) {
          this.gateProtectedContent('checking');
          this.rotateAuthenticatedRequests();
          await this.queries.cancelAndClear();
          if (
            controller.signal.aborted ||
            generation !== this.validationGeneration
          )
            return null;
          clearCsrfToken();
          this.queries.use(nextPartition);
        }
        this.establishSession(session);
        return session;
      } catch (error) {
        if (
          controller.signal.aborted ||
          generation !== this.validationGeneration ||
          abortError(error)
        ) {
          return null;
        }
        if (unauthorizedError(error)) {
          await this.transitionToAnonymous();
          return null;
        }
        if (
          this.unauthorizedHandled &&
          this.snapshotValue.phase !== 'authenticated'
        )
          return null;
        if (authenticatedSnapshot) {
          this.apply({
            type: 'validation-error',
            error:
              error instanceof Error
                ? error.message
                : 'The current session could not be loaded.'
          });
          return null;
        }
        this.gateProtectedContent(
          'unavailable',
          error instanceof Error
            ? error.message
            : 'The current session could not be loaded.'
        );
        this.rotateAuthenticatedRequests();
        await this.queries.cancelAndClear();
        if (
          controller.signal.aborted ||
          generation !== this.validationGeneration
        )
          return null;
        clearCsrfToken();
        this.queries.rotateAnonymous();
        return null;
      } finally {
        if (generation === this.validationGeneration) {
          this.activeValidation = null;
          if (this.sessionController === controller)
            this.sessionController = null;
        }
      }
    })();
    this.activeValidation = validation;
    return validation;
  }

  async ensureFreshSession(): Promise<void> {
    if (
      !this.snapshotValue.user ||
      this.snapshotValue.phase !== 'authenticated'
    ) {
      throw new DOMException(
        'No authenticated principal is active.',
        'AbortError'
      );
    }
    const startingPartition = this.queries.current();
    if (
      sessionIsFresh(
        this.snapshotValue.lastValidatedAt,
        Boolean(getCsrfToken())
      )
    )
      return;
    const session = await (this.activeValidation ?? this.validateSession());
    if (!session)
      throw new DOMException(
        'Session validation did not complete.',
        'AbortError'
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

  async prepareRequest(request: Request): Promise<Request> {
    if (
      isAuthenticationEndpoint(request) ||
      isSessionValidationEndpoint(request)
    ) {
      return request;
    }
    const mutation = isMutationRequest(request);
    if (mutation && !isCurrentSessionDeletion(request))
      await this.ensureFreshSession();
    const generation = this.authenticatedRequestGeneration;
    const signal = AbortSignal.any([
      request.signal,
      this.authenticatedRequestController.signal
    ]);
    const headers = new Headers(request.headers);
    if (mutation) {
      const csrf = getCsrfToken();
      if (csrf) headers.set('x-csrf-token', csrf);
    }
    const prepared = new Request(request, { headers, signal });
    this.requestGenerations.set(request, generation);
    this.requestGenerations.set(prepared, generation);
    return prepared;
  }

  async handleResponse(request: Request, response: Response): Promise<void> {
    const requestGeneration = this.requestGenerations.get(request);
    if (requestGeneration === this.authenticatedRequestGeneration) {
      const rotatedCsrf = response.headers.get('x-csrf-token');
      if (rotatedCsrf) setCsrfToken(rotatedCsrf);
    }
    if (response.status === 401) await this.handleUnauthorized(request);
  }

  async handleUnauthorized(request: Request): Promise<void> {
    if (
      isAuthenticationEndpoint(request) ||
      isSessionValidationEndpoint(request) ||
      isCurrentSessionDeletion(request)
    ) {
      return;
    }
    const requestGeneration = this.requestGenerations.get(request);
    if (
      requestGeneration !== undefined &&
      requestGeneration !== this.authenticatedRequestGeneration
    ) {
      return;
    }
    // A 401 can belong to a request sent with a cookie that another tab has
    // since replaced. Validate the browser's current cookie before deciding
    // that this tab has become anonymous.
    if (this.boundary) {
      await this.validateSession({ passive: true });
      return;
    }
    await this.transitionToAnonymous();
  }

  async principalInvalidated(): Promise<void> {
    await this.transitionToAnonymous();
  }

  async signOut(
    request: PrincipalExitRequest,
    destination = '/login'
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
    this.rotateAuthenticatedRequests();
  }

  private async runPrincipalExit(
    request: PrincipalExitRequest
  ): Promise<boolean> {
    this.principalExitController?.abort();
    this.abortSessionValidation();
    const controller = new AbortController();
    this.principalExitController = controller;
    this.gateProtectedContent('transitioning');
    this.rotateAuthenticatedRequests();
    await this.queries.cancelAndClear();
    this.queries.rotateAnonymous();
    try {
      await request(controller.signal);
      if (controller.signal.aborted) return false;
      clearCsrfToken();
      this.apply({ type: 'anonymous' });
      return true;
    } catch (error) {
      if (controller.signal.aborted || abortError(error)) return false;
      await this.validateSession();
      if (
        controller.signal.aborted ||
        (this.unauthorizedHandled && !this.snapshotValue.user)
      )
        return false;
      this.apply({
        type: 'principal-exit-error',
        error:
          error instanceof Error
            ? error.message
            : 'The sign-out request could not be completed.'
      });
      throw error;
    } finally {
      if (this.principalExitController === controller)
        this.principalExitController = null;
    }
  }

  private transitionToAnonymous(): Promise<void> {
    if (this.unauthorizedTransition) return this.unauthorizedTransition;
    const sessionExpired = this.snapshotValue.phase === 'authenticated';
    this.unauthorizedHandled = true;
    this.gateProtectedContent('transitioning');
    this.authenticationController?.abort();
    this.abortSessionValidation();
    this.principalExitController?.abort();
    this.rotateAuthenticatedRequests();
    clearCsrfToken();
    this.queries.rotateAnonymous();

    const boundary = this.boundary;
    const boundaryGeneration = this.boundaryGeneration;
    const controller = new AbortController();
    this.transitionController?.abort();
    this.transitionController = controller;
    this.unauthorizedTransition = (async () => {
      await this.queries.cancelAndClear();
      if (
        controller.signal.aborted ||
        boundaryGeneration !== this.boundaryGeneration
      )
        return;
      if (!boundary) {
        this.apply({ type: 'anonymous' });
        return;
      }
      const destination = await boundary.unauthenticatedDestination(
        controller.signal,
        sessionExpired
      );
      if (
        controller.signal.aborted ||
        boundaryGeneration !== this.boundaryGeneration
      )
        return;
      await boundary.navigate(destination);
    })()
      .catch((error) => {
        if (!controller.signal.aborted && !abortError(error)) {
          this.apply({
            type: 'gate',
            phase: 'unavailable',
            error:
              error instanceof Error
                ? error.message
                : 'The login destination could not be loaded.'
          });
        }
      })
      .finally(() => {
        if (this.transitionController === controller) {
          this.transitionController = null;
          this.unauthorizedTransition = null;
        }
      });
    return this.unauthorizedTransition;
  }

  private abortBoundaryWork(): void {
    this.abortSessionValidation();
    this.transitionController?.abort();
    this.transitionController = null;
    this.unauthorizedTransition = null;
  }

  private abortSessionValidation(): void {
    this.sessionController?.abort();
    this.sessionController = null;
    this.activeValidation = null;
    this.validationGeneration++;
  }

  private principalPartition(user: AuthenticatedUser): string {
    return `principal:${user.id}:${user.role}`;
  }

  private rotateAuthenticatedRequests(): void {
    this.authenticatedRequestController.abort();
    this.authenticatedRequestController = new AbortController();
    this.authenticatedRequestGeneration++;
  }

  private gateProtectedContent(
    phase: PrincipalAbsentSnapshot['phase'],
    error = ''
  ): void {
    this.apply({ type: 'gate', phase, error });
  }

  private apply(action: AuthenticationAction): void {
    this.setSnapshot(reduceAuthentication(this.snapshotValue, action));
  }

  private setSnapshot(snapshot: AuthenticationSnapshot): void {
    this.snapshotValue = snapshot;
    for (const listener of this.listeners) listener(snapshot);
  }
}

export const authLifecycle = new AuthenticationLifecycle();
