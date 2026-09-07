export const invitationKeys = {
  page: (cursor?: string) => ['invitations', 'page', cursor ?? 'first'] as const
};
