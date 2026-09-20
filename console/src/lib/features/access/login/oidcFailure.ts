const messages: Record<string, string> = {
  cancelled:
    'Single sign-on was cancelled. Start a new sign-in attempt when you are ready.',
  expired:
    'This verification expired or was already used. Start a new sign-in or identity verification attempt.',
  denied:
    'Your identity is not authorized. Ask an installation owner to check your access and OIDC role mappings.',
  link: 'This identity could not be linked or verified. Sign in to the existing account and use a linked identity from your profile.',
  provider:
    'Single sign-on could not be verified. Try a new sign-in attempt or contact your installation owner.'
};

export function oidcFailureMessage(reason: string | null): string {
  return reason && Object.hasOwn(messages, reason) ? messages[reason]! : '';
}
