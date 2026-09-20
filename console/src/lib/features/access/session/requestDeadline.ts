/** Only session verification and sign-in capability discovery use this deadline. */
export const AUTHENTICATION_DEADLINE_MS = 10_000;

export async function withAuthenticationDeadline<T>(
  work: (signal: AbortSignal) => Promise<T>,
  signal?: AbortSignal
): Promise<T> {
  const deadline = new AbortController();
  const combined = signal
    ? AbortSignal.any([signal, deadline.signal])
    : deadline.signal;
  const timer = setTimeout(() => {
    deadline.abort(
      new DOMException(
        'Authentication verification timed out. Check your connection and retry.',
        'TimeoutError'
      )
    );
  }, AUTHENTICATION_DEADLINE_MS);
  let onAbort: () => void = () => {};
  const interrupted = new Promise<never>((_, reject) => {
    onAbort = () => reject(combined.reason);
    if (combined.aborted) onAbort();
    else combined.addEventListener('abort', onAbort, { once: true });
  });
  try {
    if (combined.aborted) return await interrupted;
    return await Promise.race([work(combined), interrupted]);
  } finally {
    clearTimeout(timer);
    combined.removeEventListener('abort', onAbort);
  }
}
