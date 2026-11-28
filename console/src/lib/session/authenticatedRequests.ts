import { ApiProblem } from '#lib/management/http.js';
import { getCsrfToken } from '#lib/session/csrf.js';
import {
  isAuthenticationEndpoint,
  isCurrentSessionDeletion,
  isMFAEndpoint,
  isMutationRequest,
  isSessionValidationEndpoint
} from '#lib/session/requestPolicy.js';

const CSRF_INVALID = 'https://openllmproxy.dev/problems/csrf_invalid';

export type RequestSession = {
  isAuthenticated(): boolean;
  ensureFresh(): Promise<void>;
  adoptRotatedToken(token: string): void;
  replaceRejectedToken(): Promise<string>;
  confirmUnauthorized(): Promise<void>;
};

export class AuthenticatedRequests {
  private controller = new AbortController();
  private generation = 0;
  private generations = new WeakMap<Request, number>();

  constructor(private readonly session: RequestSession) {}

  abort(): void {
    this.controller.abort();
    this.controller = new AbortController();
    this.supersede();
  }

  supersede(): void {
    this.generation++;
  }

  reset(): void {
    this.generation = 0;
    this.generations = new WeakMap();
  }

  async prepare(request: Request): Promise<Request> {
    if (
      (isAuthenticationEndpoint(request) &&
        !(isMFAEndpoint(request) && this.session.isAuthenticated())) ||
      isSessionValidationEndpoint(request)
    ) {
      return request;
    }
    const mutation = isMutationRequest(request);
    if (mutation && !isCurrentSessionDeletion(request))
      await this.session.ensureFresh();
    const generation = this.generation;
    const signal = AbortSignal.any([request.signal, this.controller.signal]);
    const headers = new Headers(request.headers);
    if (mutation) {
      const csrf = getCsrfToken();
      if (csrf) headers.set('x-csrf-token', csrf);
    }
    const prepared = new Request(request, { headers, signal });
    this.generations.set(request, generation);
    this.generations.set(prepared, generation);
    return prepared;
  }

  async handleResponse(request: Request, response: Response): Promise<void> {
    const generation = this.generations.get(request);
    if (generation === this.generation) {
      const rotatedCsrf = response.headers.get('x-csrf-token');
      if (response.ok && rotatedCsrf && rotatedCsrf !== getCsrfToken())
        this.session.adoptRotatedToken(rotatedCsrf);
      if (response.status === 403)
        await this.rejectInvalidToken(generation, response);
    }
    if (response.status === 401) await this.handleUnauthorized(request);
  }

  private async rejectInvalidToken(
    generation: number,
    response: Response
  ): Promise<void> {
    const problem: unknown = await response
      .clone()
      .json()
      .catch(() => null);
    if (
      generation !== this.generation ||
      !problem ||
      typeof problem !== 'object' ||
      !('type' in problem) ||
      problem.type !== CSRF_INVALID
    ) {
      return;
    }
    const validationError = await this.session.replaceRejectedToken();
    throw new ApiProblem({
      status: 403,
      type: CSRF_INVALID,
      title: 'Session verification required',
      detail:
        validationError ||
        'Your session was refreshed. Review your changes and retry the action.'
    });
  }

  private async handleUnauthorized(request: Request): Promise<void> {
    if (
      isAuthenticationEndpoint(request) ||
      isSessionValidationEndpoint(request) ||
      isCurrentSessionDeletion(request)
    ) {
      return;
    }
    const generation = this.generations.get(request);
    if (generation !== undefined && generation !== this.generation) return;
    await this.session.confirmUnauthorized();
  }
}
