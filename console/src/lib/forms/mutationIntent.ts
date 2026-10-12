import { snapshotNativeJSON, stringifyNativeJSON } from '$lib/json/nativeJson';

/** One form's logical submission. An unchanged retry keeps both its request
 * identity and constructed payload (including destination-generated dates).
 * Changing input starts a new submission; a committed write ends the old one.
 * No request is retried automatically and nothing is persisted. */
export class MutationIntent {
  private pending?: { intent: unknown; input: unknown; requestId: string };

  prepare<T>(input: T): { input: T; requestId: string };
  prepare<T, I>(intent: I, build: () => T): { input: T; requestId: string };
  prepare<T, I>(intent: I, build?: () => T) {
    if (
      !this.pending ||
      stringifyNativeJSON(this.pending.intent) !== stringifyNativeJSON(intent)
    ) {
      const captured = snapshotNativeJSON(intent);
      this.pending = {
        intent: captured,
        input: build ? snapshotNativeJSON(build()) : captured,
        requestId: crypto.randomUUID()
      };
    }
    return {
      input: this.pending.input as T,
      requestId: this.pending.requestId
    };
  }

  reset() {
    this.pending = undefined;
  }

  get unresolved() {
    return this.pending !== undefined;
  }

  /** Own the write only. Refresh and navigation follow its successful response. */
  async submit<I, R>(
    input: I,
    send: (input: I, requestId: string) => Promise<R>
  ): Promise<R> {
    const submission = this.prepare(input);
    const result = await send(submission.input, submission.requestId);
    if (this.pending?.requestId === submission.requestId) this.reset();
    return result;
  }
}

/** A bulk selection can have several unresolved writes at once. Keep their
 * receipts independent and discard completed entries rather than retaining
 * a history of requests. The mounted form owns and clears this collection. */
export class MutationIntents {
  private pending = new Map<string, MutationIntent>();

  async submit<I, R>(
    key: string,
    input: I,
    send: (input: I, requestId: string) => Promise<R>
  ): Promise<R> {
    const intent = this.pending.get(key) ?? new MutationIntent();
    this.pending.set(key, intent);
    const result = await intent.submit(input, send);
    if (this.pending.get(key) === intent && !intent.unresolved)
      this.pending.delete(key);
    return result;
  }

  reset() {
    for (const intent of this.pending.values()) intent.reset();
    this.pending.clear();
  }
}
