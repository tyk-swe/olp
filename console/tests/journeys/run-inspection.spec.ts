import { test } from '../playwright';
import { signInAsOwner } from './fixtures';
import { verifyRunInspection } from './run-inspection';

test('operators inspect gateway runs, approvals and linked spans', async ({
  page
}) => {
  test.setTimeout(180_000);
  await signInAsOwner(page);
  await verifyRunInspection(page, test.info());
});
