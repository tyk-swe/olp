import type { MFAChallenge, MFAEnrollment, MFAVerification } from './api';
type Prompt = {
  id: number;
  challenge?: MFAChallenge;
  enrollment?: MFAEnrollment;
  finish: (result?: MFAVerification, error?: Error) => void;
};
export const mfaPrompt = $state<{ current: Prompt | null }>({ current: null });
let nextID = 0;
export function confirmMFA(
  value: { challenge?: MFAChallenge; enrollment?: MFAEnrollment },
  signal?: AbortSignal
): Promise<MFAVerification> {
  if (mfaPrompt.current)
    return Promise.reject(new Error('Finish the current verification first.'));
  if (signal?.aborted)
    return Promise.reject(new Error('Verification was cancelled.'));
  return new Promise((resolve, reject) => {
    const id = ++nextID;
    const finish = (result?: MFAVerification, error?: Error) => {
      if (mfaPrompt.current?.id !== id) return;
      signal?.removeEventListener('abort', abort);
      mfaPrompt.current = null;
      if (error) reject(error);
      else resolve(result);
    };
    const abort = () =>
      finish(undefined, new Error('Verification was cancelled.'));
    mfaPrompt.current = { id, ...value, finish };
    signal?.addEventListener('abort', abort, { once: true });
  });
}
