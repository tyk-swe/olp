import { expect, test, type Page } from '../playwright';

export async function verifyDraftSave(
  page: Page,
  kind: 'route' | 'provider',
  original: string
) {
  const id = new URL(page.url()).pathname.split('/').at(-1)!;
  const endpoint = `/api/v3/${kind === 'route' ? 'route-drafts' : 'providers'}/${id}`;
  const input = page.getByLabel(
    kind === 'route' ? 'Public model slug' : 'Provider name'
  );
  const method = kind === 'route' ? 'PUT' : 'PATCH';
  const saved = Promise.withResolvers<string>();
  const release = Promise.withResolvers<void>();
  const pattern = `**${endpoint}`;
  await page.route(pattern, async (route) => {
    if (route.request().method() !== method) return route.continue();
    const response = await route.fetch();
    expect(response.ok()).toBe(true);
    saved.resolve((await response.json()).etag);
    await release.promise;
    await route.fulfill({ response });
  });
  try {
    await input.fill(`${original}-saved`);
    await page.getByRole('button', { name: 'Save draft', exact: true }).click();
    const etag = await saved.promise;
    await expect(input).toBeEnabled();
    await input.fill(original);
    release.resolve();
    await expect(
      page.getByText('Draft saved. You have additional unsaved changes.', {
        exact: true
      })
    ).toBeVisible();
    await expect(input).toHaveValue(original);
    if (kind === 'route') {
      for (const name of [
        'Simulate order',
        'Validate draft',
        'Activate route'
      ]) {
        await expect(
          page.getByRole('button', { name, exact: true })
        ).toBeDisabled();
      }
    }
    const dialogSeen = Promise.withResolvers<string>();
    page.once('dialog', async (dialog) => {
      dialogSeen.resolve(dialog.message());
      await dialog.dismiss();
    });
    const before = page.url();
    await page
      .getByRole('link', {
        name: kind === 'route' ? 'Cancel' : 'All providers',
        exact: true
      })
      .click();
    expect(await dialogSeen.promise).toBe('Discard unsaved changes?');
    await expect(page).toHaveURL(before);
    await page.screenshot({
      path: test.info().outputPath(`draft-save-unsaved-${kind}.png`),
      fullPage: true
    });
    await page.unroute(pattern);
    const nextRequest = page.waitForRequest(
      (request) =>
        new URL(request.url()).pathname === endpoint &&
        request.method() === method
    );
    await page.getByRole('button', { name: 'Save draft', exact: true }).click();
    expect((await nextRequest).headers()['if-match']).toBe(`"${etag}"`);
    await expect(
      page.getByRole('button', { name: 'Save draft', exact: true })
    ).toBeEnabled();
    await expect(
      page.getByText('Draft saved. You have additional unsaved changes.', {
        exact: true
      })
    ).toHaveCount(0);
    await expect(input).toHaveValue(original);
    const persisted = await page.request.get(endpoint);
    expect(persisted.ok()).toBe(true);
    expect((await persisted.json())[kind === 'route' ? 'slug' : 'name']).toBe(
      original
    );
  } finally {
    release.resolve();
    await page.unroute(pattern);
  }
}
