export const FIXED_ROLES = [
  'owner',
  'operator',
  'developer',
  'viewer'
] as const;

export function accessErrorMessage(error: unknown): string {
  return error instanceof Error
    ? error.message
    : 'The control API could not complete the request.';
}
