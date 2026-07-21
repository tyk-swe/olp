export { result as requireResponseData } from '../http';
export { collectCursorPages, type CursorPage } from '../pagination';

export type ReadSignal = AbortSignal | { readonly signal: AbortSignal };

export function getAbortSignal(input?: ReadSignal): AbortSignal | undefined {
  if (input && 'signal' in input) return input.signal;
  return input;
}
