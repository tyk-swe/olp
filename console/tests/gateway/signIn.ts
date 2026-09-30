import type { Page } from '../playwright';
import { signInOwner } from '../helpers/owner';

export const gatewayOwner = {
  name: 'Owner',
  email: 'owner@example.com',
  password: 'a long browser test password'
};

export function signInGatewayOwner(
  page: Page,
  options: Parameters<typeof signInOwner>[2] = {}
) {
  return signInOwner(page, gatewayOwner, options);
}
