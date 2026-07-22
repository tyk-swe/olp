import type { QueryClient } from '@tanstack/svelte-query';
import { clearCsrfToken, getCsrfToken, setCsrfToken } from '$lib/api/session';
import type { FixedRole } from './authorization';

export type AuthenticatedUser = {
  id: string;
  email: string;
  display_name: string;
  role: FixedRole;
};

export type AuthenticatedSession = {
  user: AuthenticatedUser;
  csrf_token: string;
};

export type AuthenticationPhase =
  | 'anonymous'
  | 'checking'
  | 'authenticated'
  | 'transitioning'
  | 'unavailable';

export type AuthenticationSnapshot = {
  phase: AuthenticationPhase;
  user: AuthenticatedUser | null;
  error: string;
  lastValidatedAt: number | null;
};

type Boundary = {
  loadSession(signal: AbortSignal): Promise<AuthenticatedSession>;
  unauthenticatedDestination(signal: AbortSignal): Promise<string>;
  loginDestination(): string;
  navigate(destination: string): Promise<void>;
};

type ValidateOptions = { passive?: boolean };

type PrincipalExitRequest = (signal: AbortSignal) => Promise<void>;
type AuthenticationRequest = (signal: AbortSignal) => Promise<AuthenticatedSession>;

const SAFE_METHODS = new Set(['GET', 'HEAD', 'OPTIONS']);
const SESSION_FRESHNESS_MS = 60_000;
const PASSIVE_COALESCE_MS = 500;

function unauthorizedError(error: unknown): boolean {
  return (error as { problem?: { status?: unknown } } | null)?.problem?.status === 401;
}

function abortError(error: unknown): boolean {
  return error instanceof Error && error.name === 'AbortError';
}

function endpoint(request: Request): { method: string; pathname: string } {
  const url = new URL(request.url);
  return { method: request.method.toUpperCase(), pathname: url.pathname };
}

function isAuthenticationEndpoint(request: Request): boolean {
  const { method, pathname } = endpoint(request);
  return (
    (method === 'GET' && pathname === '/api/v1/setup/status') ||
    (method === 'POST' && pathname === '/api/v1/setup') ||
    (method === 'POST' && pathname === '/api/v1/sessions') ||
    (method === 'POST' && pathname === '/api/v1/invitations/accept') ||
    (method === 'GET' && pathname === '/api/v1/oidc/login') ||
    (method === 'GET' && pathname === '/api/v1/oidc/callback')
  );
}

function isSessionValidationEndpoint(request: Request): boolean {
  const { method, pathname } = endpoint(request);
  return method === 'GET' && pathname === '/api/v1/sessions/current';
}

function isCurrentSessionDeletion(request: Request): boolean {
  const { method, pathname } = endpoint(request);
  return method === 'DELETE' && pathname === '/api/v1/sessions/current';
}

function combineSignals(...signals: AbortSignal[]): AbortSignal {
  if (typeof AbortSignal.any === 'function') return AbortSignal.any(signals);
  const controller = new AbortController();
  for (const signal of signals) {
    if (signal.aborted) {
      controller.abort(signal.reason);
      break;
    }
    signal.addEventListener('abort', () => controller.abort(signal.reason), { once: true });
  }
  return controller.signal;
}

function stableSerialize(value: unknown): string {
  if (value === null || typeof value !== 'object') {
    if (typeof value === 'bigint') return JSON.stringify(value.toString());
    if (value === undefined) return 'undefined';
    return JSON.stringify(value) ?? String(value);
  }
  if (value instanceof Date) return `date:${value.toISOString()}`;
  if (Array.isArray(value)) return `[${value.map(stableSerialize).join(',')}]`;
  return `{${Object.keys(value as Record<string, unknown>)
    .sort()
    .map((key) => `${JSON.stringify(key)}:${stableSerialize((value as Record<string, unknown>)[key])}`)
    .join(',')}}`;
}

export class AuthenticationLifecycle {
  private queryClient: QueryClient | null = null;
  private boundary: Boundary | null = null;
  private boundaryGeneration = 0;
  private listeners = new Set<(snapshot: AuthenticationSnapshot) => void>();
  private snapshotValue: AuthenticationSnapshot = {
    phase: 'anonymous',
    user: null,
    error: '',
    lastValidatedAt: null
  };
  private partitionGeneration = 0;
  private partition = 'anonymous:0';
  private sessionController: AbortController | null = null;
  private transitionController: AbortController | null = null;
  private authenticationController: AbortController | null = null;
  private principalExitController: AbortController | null = null;
  private authenticatedRequestController = new AbortController();
  private validationGeneration = 0;
  private authenticationGeneration = 0;
  private activeValidation: Promise<AuthenticatedSession | null> | null = null;
  private activeValidationStartedAt = 0;
  private unauthorizedTransition: Promise<void> | null = null;
  private unauthorizedHandled = false;

  attachQueryClient(client: QueryClient): () => void {
    this.queryClient = client;
    return () => {
      if (this.queryClient === client) this.queryClient = null;
    };
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
    this.setSnapshot({ phase: 'checking', user: null, error: '', lastValidatedAt: null });
  }

  queryKeyHash(key: readonly unknown[]): string {
    return `${this.partition}|${stableSerialize(key)}`;
  }

  async authenticate(request: AuthenticationRequest): Promise<AuthenticatedSession> {
    const generation = ++this.authenticationGeneration;
    this.authenticationController?.abort();
    this.sessionController?.abort();
    this.transitionController?.abort();
    this.principalExitController?.abort();
    const controller = new AbortController();
    this.authenticationController = controller;
    this.gateProtectedContent('transitioning');
    this.rotateAuthenticatedRequests();
    await this.cancelAndClearQueries();
    if (generation !== this.authenticationGeneration || controller.signal.aborted) {
      throw new DOMException('Authentication was superseded.', 'AbortError');
    }
    clearCsrfToken();
    this.rotatePartition();
    const session = await request(controller.signal);
    if (generation !== this.authenticationGeneration || controller.signal.aborted) {
      throw new DOMException('Authentication was superseded.', 'AbortError');
    }
    this.establishSession(session);
    return session;
  }

  establishSession(session: AuthenticatedSession): void {
    const partition = this.principalPartition(session.user);
    if (partition !== this.partition) this.partition = partition;
    if (session.csrf_token) setCsrfToken(session.csrf_token);
    else clearCsrfToken();
    this.unauthorizedHandled = false;
    this.setSnapshot({
      phase: 'authenticated',
      user: session.user,
      error: '',
      lastValidatedAt: Date.now()
    });
  }

  async validateSession(options: ValidateOptions = {}): Promise<AuthenticatedSession | null> {
    const boundary = this.boundary;
    if (!boundary) return null;
    if (options.passive && this.snapshotValue.phase !== 'authenticated') {
      return this.activeValidation;
    }
    const now = Date.now();
    if (
      options.passive &&
      this.activeValidation &&
      now - this.activeValidationStartedAt < PASSIVE_COALESCE_MS
    ) {
      return this.activeValidation;
    }

    this.sessionController?.abort();
    const controller = new AbortController();
    this.sessionController = controller;
    const generation = ++this.validationGeneration;
    this.activeValidationStartedAt = now;
    this.unauthorizedHandled = false;
    const authenticatedSnapshot =
      this.snapshotValue.phase === 'authenticated' && this.snapshotValue.user
        ? this.snapshotValue
        : null;
    if (authenticatedSnapshot) {
      this.setSnapshot({ ...authenticatedSnapshot, error: '' });
    } else {
      this.setSnapshot({ ...this.snapshotValue, phase: 'checking', error: '' });
    }

    const validation = (async (): Promise<AuthenticatedSession | null> => {
      try {
        const session = await boundary.loadSession(controller.signal);
        if (controller.signal.aborted || generation !== this.validationGeneration) return null;
        const nextPartition = this.principalPartition(session.user);
        if (nextPartition !== this.partition) {
          this.gateProtectedContent('checking');
          this.rotateAuthenticatedRequests();
          await this.cancelAndClearQueries();
          clearCsrfToken();
          this.partition = nextPartition;
        }
        this.establishSession(session);
        return session;
      } catch (error) {
        if (controller.signal.aborted || generation !== this.validationGeneration || abortError(error)) {
          return null;
        }
        if (unauthorizedError(error)) {
          await this.transitionToAnonymous();
          return null;
        }
        if (this.unauthorizedHandled && this.snapshotValue.phase !== 'authenticated') return null;
        if (authenticatedSnapshot) {
          this.setSnapshot({
            ...authenticatedSnapshot,
            error:
              error instanceof Error
                ? error.message
                : 'The current session could not be loaded.'
          });
          return null;
        }
        this.gateProtectedContent('unavailable', error instanceof Error ? error.message : 'The current session could not be loaded.');
        this.rotateAuthenticatedRequests();
        await this.cancelAndClearQueries();
        clearCsrfToken();
        this.rotatePartition();
        return null;
      } finally {
        if (generation === this.validationGeneration) {
          this.activeValidation = null;
          if (this.sessionController === controller) this.sessionController = null;
        }
      }
    })();
    this.activeValidation = validation;
    return validation;
  }

  async ensureFreshSession(): Promise<void> {
    if (!this.snapshotValue.user || this.snapshotValue.phase !== 'authenticated') {
      throw new DOMException('No authenticated principal is active.', 'AbortError');
    }
    const age = Date.now() - (this.snapshotValue.lastValidatedAt ?? 0);
    if (getCsrfToken() && age <= SESSION_FRESHNESS_MS) return;
    const session = await (this.activeValidation ?? this.validateSession());
    if (!session) throw new DOMException('Session validation did not complete.', 'AbortError');
    if (!getCsrfToken()) {
      throw new DOMException(
        'This session cannot make changes until you sign in again.',
        'InvalidStateError'
      );
    }
  }

  async prepareRequest(request: Request): Promise<Request> {
    if (isAuthenticationEndpoint(request) || isSessionValidationEndpoint(request)) {
      return request;
    }
    const mutation = !SAFE_METHODS.has(request.method.toUpperCase());
    if (mutation && !isCurrentSessionDeletion(request)) await this.ensureFreshSession();
    const signal = combineSignals(request.signal, this.authenticatedRequestController.signal);
    const headers = new Headers(request.headers);
    if (mutation) {
      const csrf = getCsrfToken();
      if (csrf) headers.set('x-csrf-token', csrf);
    }
    return new Request(request, { headers, signal });
  }

  async handleUnauthorized(request: Request): Promise<void> {
    if (
      isAuthenticationEndpoint(request) ||
      isSessionValidationEndpoint(request) ||
      isCurrentSessionDeletion(request)
    ) {
      return;
    }
    await this.transitionToAnonymous();
  }

  async principalInvalidated(): Promise<void> {
    await this.transitionToAnonymous();
  }

  async signOut(request: PrincipalExitRequest, destination = '/login'): Promise<void> {
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

  private async runPrincipalExit(request: PrincipalExitRequest): Promise<boolean> {
    this.principalExitController?.abort();
    const controller = new AbortController();
    this.principalExitController = controller;
    this.gateProtectedContent('transitioning');
    this.rotateAuthenticatedRequests();
    await this.cancelAndClearQueries();
    this.rotatePartition();
    try {
      await request(controller.signal);
      if (controller.signal.aborted) return false;
      clearCsrfToken();
      this.setSnapshot({ phase: 'anonymous', user: null, error: '', lastValidatedAt: null });
      return true;
    } catch (error) {
      if (controller.signal.aborted || abortError(error)) return false;
      if (!controller.signal.aborted) await this.validateSession();
      if (this.unauthorizedHandled && !this.snapshotValue.user) return false;
      throw error;
    } finally {
      if (this.principalExitController === controller) this.principalExitController = null;
    }
  }

  private transitionToAnonymous(): Promise<void> {
    if (this.unauthorizedTransition) return this.unauthorizedTransition;
    this.unauthorizedHandled = true;
    this.gateProtectedContent('transitioning');
    this.authenticationController?.abort();
    this.sessionController?.abort();
    this.principalExitController?.abort();
    this.rotateAuthenticatedRequests();
    clearCsrfToken();
    this.rotatePartition();

    const boundary = this.boundary;
    const boundaryGeneration = this.boundaryGeneration;
    const controller = new AbortController();
    this.transitionController?.abort();
    this.transitionController = controller;
    this.unauthorizedTransition = (async () => {
      await this.cancelAndClearQueries();
      if (!boundary || controller.signal.aborted || boundaryGeneration !== this.boundaryGeneration) {
        this.setSnapshot({ phase: 'anonymous', user: null, error: '', lastValidatedAt: null });
        return;
      }
      const destination = await boundary.unauthenticatedDestination(controller.signal);
      if (controller.signal.aborted || boundaryGeneration !== this.boundaryGeneration) return;
      await boundary.navigate(destination);
    })()
      .catch((error) => {
        if (!controller.signal.aborted && !abortError(error)) {
          this.setSnapshot({
            phase: 'unavailable',
            user: null,
            error: error instanceof Error ? error.message : 'The login destination could not be loaded.',
            lastValidatedAt: null
          });
        }
      })
      .finally(() => {
        if (this.transitionController === controller) this.transitionController = null;
        this.unauthorizedTransition = null;
      });
    return this.unauthorizedTransition;
  }

  private abortBoundaryWork(): void {
    this.sessionController?.abort();
    this.transitionController?.abort();
    this.sessionController = null;
    this.transitionController = null;
    this.activeValidation = null;
    this.unauthorizedTransition = null;
  }

  private principalPartition(user: AuthenticatedUser): string {
    return `principal:${user.id}:${user.role}`;
  }

  private rotatePartition(): void {
    this.partition = `anonymous:${++this.partitionGeneration}`;
  }

  private rotateAuthenticatedRequests(): void {
    this.authenticatedRequestController.abort();
    this.authenticatedRequestController = new AbortController();
  }

  private gateProtectedContent(phase: AuthenticationPhase, error = ''): void {
    this.setSnapshot({ phase, user: null, error, lastValidatedAt: null });
  }

  private setSnapshot(snapshot: AuthenticationSnapshot): void {
    this.snapshotValue = snapshot;
    for (const listener of this.listeners) listener(snapshot);
  }

  private async cancelAndClearQueries(): Promise<void> {
    const client = this.queryClient;
    if (!client) return;
    try {
      await client.cancelQueries();
    } finally {
      client.clear();
    }
  }
}

export const authLifecycle = new AuthenticationLifecycle();
export const authenticatedQueryKey = <T extends readonly unknown[]>(key: T): T => key;

export const authLifecycleTesting = {
  isAuthenticationEndpoint,
  isSessionValidationEndpoint,
  isCurrentSessionDeletion
};
