export function healthTone(value?: string | null) {
  const state = value?.toLowerCase();
  if (!state) return 'warning';
  if (['healthy', 'ok', 'active', 'passing', 'drained'].includes(state))
    return 'success';
  // `not_configured` is a deployment choice rather than a fault: distributed
  // limits report it when no limiter backend is configured at all, and the
  // gateway is then running exactly as installed. `unavailable` is the
  // opposite case — a limiter is configured and cannot be reached — so it
  // falls through to danger with the other hard failures.
  if (
    [
      'degraded',
      'stale',
      'unknown',
      'not_checked',
      'backlogged',
      'unavailable_lkg',
      'not_configured'
    ].includes(state)
  )
    return 'warning';
  return 'danger';
}
