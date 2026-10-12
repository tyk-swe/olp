import { onDestroy } from 'svelte';
import { authLifecycle } from '$lib/features/access/session/lifecycle';
import { ApiProblem, errorMessage } from '$lib/api/http';
import type { Provider } from './api/providers';
import {
  cancelGrantEnrollment,
  continueGrantEnrollment,
  pollGrantEnrollment,
  startGrantEnrollment,
  type GrantEnrollment,
  type GrantEnrollmentCompletion
} from './api/grants';

type CompletionContext = {
  secret: boolean;
  /** Check after asynchronous feature-specific follow-up work. */
  isCurrent: () => boolean;
};

/** Owns one ephemeral enrollment, not provider onboarding or activation.
 * All consumers share polling, one-use continuation, cancellation and stale
 * completion rules; their completion hooks retain their distinct workflows. */
export class GrantEnrollmentState {
  enrollment = $state<GrantEnrollment | null>(null);
  input = $state('');
  busy = $state('');
  private epoch = 0;
  private active = true;

  constructor(
    private hooks: {
      allowed: () => boolean;
      reportTransientErrors?: boolean;
      error: (message: string, failure?: unknown) => void;
      completed: (
        completion: GrantEnrollmentCompletion,
        context: CompletionContext
      ) => void | Promise<void>;
    }
  ) {
    const unsubscribe = authLifecycle.onPrincipalChange(() => this.abandon());
    $effect(() => {
      if (!hooks.allowed()) this.abandon();
    });
    onDestroy(() => {
      unsubscribe();
      this.active = false;
      this.abandon();
    });
  }

  private current(epoch: number) {
    return this.active && this.epoch === epoch && this.hooks.allowed();
  }

  /** Clear sensitive input immediately; remote cleanup is best effort and
   * the backend expiry remains authoritative when cancellation is refused. */
  abandon() {
    const enrollment = this.enrollment;
    this.epoch++;
    this.enrollment = null;
    this.input = '';
    this.busy = '';
    if (enrollment)
      void Promise.resolve()
        .then(() => cancelGrantEnrollment(enrollment))
        .catch(() => {});
  }

  async start(provider: Pick<Provider, 'id' | 'etag'>, slotId?: string) {
    if (this.busy || this.enrollment || !this.hooks.allowed()) return;
    const epoch = ++this.epoch;
    this.busy = 'grant-start';
    this.hooks.error('');
    try {
      const result = slotId
        ? await startGrantEnrollment(provider, slotId)
        : await startGrantEnrollment(provider);
      if (this.current(epoch)) this.enrollment = result;
      else
        void Promise.resolve()
          .then(() => cancelGrantEnrollment(result))
          .catch(() => {});
    } catch (error) {
      if (this.current(epoch)) this.hooks.error(errorMessage(error), error);
    } finally {
      if (this.epoch === epoch) this.busy = '';
    }
  }

  private async completed(
    completion: GrantEnrollmentCompletion,
    enrollment: GrantEnrollment,
    epoch: number
  ) {
    if (!this.current(epoch)) return;
    this.enrollment = null;
    this.input = '';
    try {
      await this.hooks.completed(completion, {
        secret: enrollment.input === 'secret',
        isCurrent: () => this.current(epoch)
      });
    } catch (error) {
      if (this.current(epoch))
        this.hooks.error(
          `Enrollment saved, but follow-up failed. ${errorMessage(error)}`,
          error
        );
    }
  }

  async continue() {
    const enrollment = this.enrollment;
    if (!enrollment || this.busy || !this.hooks.allowed()) return;
    const input = this.input.trim();
    if (!input) {
      this.hooks.error(
        'Paste the callback URL, code or key the upstream issued.'
      );
      return;
    }
    const epoch = this.epoch;
    this.busy = 'grant';
    this.hooks.error('');
    // Continuations are one-use even when their response is lost. Do not
    // retain or resubmit pasted secrets after sending them.
    this.input = '';
    try {
      const completion = await continueGrantEnrollment(enrollment, input);
      await this.completed(completion, enrollment, epoch);
    } catch (error) {
      if (this.current(epoch)) this.hooks.error(errorMessage(error), error);
    } finally {
      if (this.epoch === epoch) {
        this.enrollment = null;
        this.busy = '';
      }
    }
  }

  async poll(): Promise<number | null> {
    const enrollment = this.enrollment;
    if (!enrollment || !this.hooks.allowed()) return null;
    const interval = enrollment.device?.interval ?? null;
    if (this.busy) return interval;
    const epoch = this.epoch;
    this.busy = 'grant-poll';
    this.hooks.error('');
    try {
      const status = await pollGrantEnrollment(enrollment);
      if (!this.current(epoch)) return null;
      if (status.status === 'pending') return status.interval ?? interval;
      if (status.status === 'completed' && status.completion)
        await this.completed(status.completion, enrollment, epoch);
      else {
        this.enrollment = null;
        this.hooks.error(
          `The device sign-in ${status.status === 'denied' ? 'was denied upstream' : 'expired before it was approved'}. Start enrollment again.`
        );
      }
      return null;
    } catch (error) {
      if (!this.current(epoch)) return null;
      const transient =
        error instanceof TypeError ||
        (error instanceof ApiProblem &&
          (error.problem.status >= 500 ||
            [408, 429].includes(error.problem.status)));
      if (!transient || this.hooks.reportTransientErrors)
        this.hooks.error(errorMessage(error));
      // Refusals end this enrollment; transport failures and transient HTTP
      // responses preserve it and the server's most recent polling interval.
      if (!transient) {
        this.enrollment = null;
        return null;
      }
      return interval;
    } finally {
      if (this.epoch === epoch) this.busy = '';
    }
  }

  async cancel() {
    const enrollment = this.enrollment;
    if (!enrollment || !this.hooks.allowed()) return;
    const epoch = ++this.epoch;
    this.enrollment = null;
    this.input = '';
    this.busy = 'grant-cancel';
    this.hooks.error('');
    try {
      await cancelGrantEnrollment(enrollment);
    } catch (error) {
      if (this.current(epoch)) this.hooks.error(errorMessage(error), error);
    } finally {
      if (this.epoch === epoch) this.busy = '';
    }
  }
}
