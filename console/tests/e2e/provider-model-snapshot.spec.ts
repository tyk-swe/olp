import {
  expect,
  failUnexpectedApiRequest,
  mockSession,
  test,
  type Page
} from '../playwright';
import { mockProviderKinds } from './provider-capabilities';
import {
  ids,
  modelRecord,
  providerRecord,
  sessionOptions
} from './gateway-access-fixtures';

type SnapshotState = {
  current: ReturnType<typeof providerRecord>;
  blockReload: boolean;
  reloadWaiting: boolean;
  releaseReload: Promise<void>;
  saveEtags: string[];
};

async function mockModelSnapshot(page: Page, state: SnapshotState) {
  await page.route(
    '**/api/v1/provider-kinds/openai/capabilities',
    async (route) => {
      await route.fulfill({
        json: {
          provider_kind: 'openai',
          capabilities: [
            { operation: 'generation', surface: 'openai', mode: 'streaming' },
            { operation: 'generation', surface: 'openai', mode: 'unary' }
          ]
        }
      });
    }
  );
  await page.route('**/api/v1/providers/**', async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    if (url.pathname === `/api/v1/providers/${ids.provider}`) {
      await route.fulfill({ json: state.current });
      return;
    }
    if (url.pathname === `/api/v1/providers/${ids.provider}/models`) {
      let etag = state.current.etag;
      if (state.blockReload) {
        state.reloadWaiting = true;
        await state.releaseReload;
        etag = '01980000-0000-7000-8000-000000000713';
      }
      const secondPage = url.searchParams.has('cursor');
      await route.fulfill({
        json: {
          provider_etag: etag,
          items: [
            {
              ...modelRecord,
              display_name: secondPage ? 'Second model' : 'First model'
            }
          ],
          next_cursor: secondPage ? null : 'second-page'
        }
      });
      return;
    }
    if (
      url.pathname === `/api/v1/providers/${ids.provider}/models/${ids.model}`
    ) {
      state.saveEtags.push((await request.allHeaders())['if-match']);
      await route.fulfill({ json: state.current });
      return;
    }
    if (
      url.pathname.endsWith('/credentials') ||
      url.pathname.endsWith('/revisions')
    ) {
      await route.fulfill({ json: { items: [], next_cursor: null } });
      return;
    }
    failUnexpectedApiRequest(route);
  });
}

test('cross-page provider changes retain editors until a matching reload succeeds', async ({
  page
}) => {
  await mockSession(page, sessionOptions);
  await mockProviderKinds(page);
  const { promise: reloadResponse, resolve: releaseReload } =
    Promise.withResolvers<void>();
  const state: SnapshotState = {
    current: providerRecord('draft', [modelRecord], {
      etag: '01980000-0000-7000-8000-000000000711',
      model_count: 2
    }),
    blockReload: false,
    reloadWaiting: false,
    releaseReload: reloadResponse,
    saveEtags: []
  };
  await mockModelSnapshot(page, state);
  try {
    await page.goto(`/providers/${ids.provider}`);
    await expect(page.getByText('First model', { exact: true })).toBeVisible();
    await page.getByLabel('Name').fill('Unsaved connector name');
    await page.getByLabel('Mode 1').selectOption('unary');
    state.current = {
      ...state.current,
      etag: '01980000-0000-7000-8000-000000000712',
      name: 'Remote provider'
    };
    await page
      .getByRole('navigation', { name: 'Provider model pages' })
      .getByRole('button', { name: 'Next' })
      .click();
    await expect(page.getByText('This item changed elsewhere.')).toBeVisible();
    await expect(page.getByText('First model', { exact: true })).toBeVisible();
    await expect(page.getByText('Second model', { exact: true })).toHaveCount(
      0
    );
    await expect(page.getByLabel('Mode 1')).toHaveValue('unary');
    state.blockReload = true;
    await page.getByRole('button', { name: 'Reload', exact: true }).click();
    await expect.poll(() => state.reloadWaiting).toBe(true);
    await expect(page.getByLabel('Name')).toHaveValue('Unsaved connector name');
    releaseReload();
    await expect(
      page.getByRole('button', { name: 'Reload', exact: true })
    ).toBeEnabled();
    await expect(page.getByLabel('Name')).toHaveValue('Unsaved connector name');
    await expect(page.getByLabel('Mode 1')).toHaveValue('unary');
    await expect(page.getByText('Second model', { exact: true })).toHaveCount(
      0
    );
    state.blockReload = false;
    await page.getByRole('button', { name: 'Reload', exact: true }).click();
    await expect(page.getByText('Second model', { exact: true })).toBeVisible();
    await expect(page.getByLabel('Name')).toHaveValue('Remote provider');
    await expect(page.getByLabel('Mode 1')).toHaveValue('streaming');
    await expect(page.getByText('This item changed elsewhere.')).toHaveCount(0);
    await page.getByRole('button', { name: 'Save capability review' }).click();
    await expect(
      page.getByText('Capability review saved with declared provenance.')
    ).toBeVisible();
    expect(state.saveEtags).toEqual(['"01980000-0000-7000-8000-000000000712"']);
  } finally {
    releaseReload();
  }
});
