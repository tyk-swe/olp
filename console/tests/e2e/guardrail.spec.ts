import { expect, test } from './journey';

test('an active keyword policy blocks matching text and allows the rest', async ({
  page
}) => {
  const project = 'browser-guardrails';
  const check = 'browser-keywords';
  const policy = 'browser-policy';

  await page.goto('/projects');
  await page.getByRole('button', { name: 'New project' }).first().click();
  const newProject = page.getByRole('dialog', { name: 'New project' });
  await newProject.getByRole('textbox', { name: 'Name' }).fill(project);
  await newProject.getByRole('button', { name: 'Create project' }).click();
  await expect(page).toHaveURL(/\/projects\/.+/);

  await page.goto('/guardrails/checks');
  await page.getByRole('button', { name: 'New check' }).click();
  const newCheck = page.getByRole('dialog', { name: 'New check' });
  await newCheck.getByRole('textbox', { name: 'Name' }).fill(check);
  await newCheck
    .getByRole('combobox', { name: 'Project' })
    .selectOption({ label: project });
  await newCheck
    .getByRole('textbox', { name: 'Configuration (JSON)' })
    .fill(JSON.stringify({ words: ['forbidden'] }));
  await newCheck.getByRole('button', { name: 'Save draft' }).click();
  await page.getByRole('button', { name: 'Activate revision 1' }).click();
  await expect(page.getByRole('row').filter({ hasText: check })).toContainText(
    'Active'
  );

  await page.goto('/guardrails/policies');
  await page.getByRole('button', { name: 'New policy' }).click();
  const newPolicy = page.getByRole('dialog', { name: 'New policy' });
  await newPolicy.getByRole('textbox', { name: 'Name' }).fill(policy);
  await newPolicy
    .getByRole('combobox', { name: 'Project' })
    .selectOption({ label: project });
  await newPolicy.getByRole('button', { name: 'Add guardrail' }).click();
  await newPolicy
    .getByRole('combobox', { name: 'Guardrail' })
    .selectOption({ label: `${check} · Revision 1` });
  await newPolicy.getByRole('button', { name: 'Save policy draft' }).click();
  await page.getByRole('button', { name: 'Activate revision' }).click();
  await expect(page.getByText('Policy activated')).toBeVisible();

  await page.goto('/guardrails/try');
  await page
    .getByRole('combobox', { name: 'Policy revision' })
    .selectOption({ label: `${policy} · Revision 1` });
  const verdict = page.getByRole('status').getByRole('heading');
  await page.getByRole('textbox', { name: 'Text' }).fill('a forbidden word');
  await page.getByRole('button', { name: 'Evaluate' }).click();
  await expect(verdict).toHaveText('Blocked');
  await page.getByRole('textbox', { name: 'Text' }).fill('an ordinary word');
  await page.getByRole('button', { name: 'Evaluate' }).click();
  await expect(verdict).toHaveText('Allowed');
});
