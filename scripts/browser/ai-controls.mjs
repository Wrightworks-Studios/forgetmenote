import {expect} from '@playwright/test';

export async function openAIControls(page) {
  await page.locator('#account-actions-toggle').click();
  await page.locator('#admin-open').click();
  const dialog = page.locator('#admin-dialog');
  await dialog.locator('[data-settings-category=ai]').click();
  await expect(dialog.locator('#instance-ai-status')).not.toHaveText('Loading…');
  await expect(dialog.locator('#instance-ai-form')).toBeVisible();
  return dialog;
}
export async function saveAIControls(dialog) {
  await dialog.getByRole('button', {name: 'Save AI settings', exact: true}).click();
  await expect(dialog.locator('#instance-ai-status')).toHaveText('AI settings saved for all notebooks.');
}
