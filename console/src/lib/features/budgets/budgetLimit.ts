/** Current allowance is separate from the permanent editable policy. */
export function currentBudgetLimit(
  window: { limit: string | null; effective_limit?: string | null } | undefined
): string | null {
  if (!window) return null;
  return window.effective_limit !== undefined
    ? window.effective_limit
    : window.limit;
}
