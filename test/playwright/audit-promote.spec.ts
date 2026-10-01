import { test, expect } from '@playwright/test';

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

async function gotoAudit(page: import('@playwright/test').Page): Promise<void> {
  await page.goto('/ui/audit');
  await page.waitForFunction(() => {
    const el = document.getElementById('auditInfo');
    return el && !el.textContent!.includes('Loading');
  }, { timeout: 15_000 });
}

async function showPromoteBar(page: import('@playwright/test').Page): Promise<void> {
  // Click the first entry thumbnail to show the promote bar
  const thumb = page.locator('.entry-thumb').first();
  await expect(thumb).toBeVisible();
  await thumb.click();
  await expect(page.locator('#promoteBar')).toBeVisible({ timeout: 5_000 });
}

async function waitForPromoteOptions(page: import('@playwright/test').Page): Promise<void> {
  await page.waitForFunction(() => {
    const sel = document.getElementById('promoteUserSelect');
    return sel && sel.options.length > 1;
  }, { timeout: 10_000 });
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

test('promoteUserSelect dropdown has "-- New User --" as first option',
  async ({ page }) => {
    await gotoAudit(page);
    await showPromoteBar(page);

    const sel = page.locator('#promoteUserSelect');
    await expect(sel).toBeVisible();

    const firstOpt = sel.locator('option:first-child');
    await expect(firstOpt).toHaveText('-- New User --');
    await expect(firstOpt).toHaveAttribute('value', 'new');
  });

test('promote bar: selecting a user hides the text input',
  async ({ page }) => {
    await gotoAudit(page);
    await showPromoteBar(page);
    await waitForPromoteOptions(page);

    const nameInput = page.locator('#promoteName');
    await expect(nameInput).toBeHidden();

    const sel = page.locator('#promoteUserSelect');
    await sel.selectOption({ label: 'Anthony' });

    await expect(nameInput).toBeHidden();
  });

test('promote bar: selecting "New User" shows and focuses the text input',
  async ({ page }) => {
    await gotoAudit(page);
    await showPromoteBar(page);
    await waitForPromoteOptions(page);

    await page.locator('#promoteUserSelect').selectOption({ label: 'Anthony' });
    let nameInput = page.locator('#promoteName');
    await expect(nameInput).toBeHidden();

    await page.locator('#promoteUserSelect').selectOption({ value: 'new' });
    await expect(nameInput).toBeVisible();
    await expect(nameInput).toBeFocused();
  });

test('promote bar: deselect button hides the promote bar',
  async ({ page }) => {
    await gotoAudit(page);
    await showPromoteBar(page);

    await page.locator('#deselectBtn').click();
    await expect(page.locator('#promoteBar')).toBeHidden();
  });

test('promote bar: Enter key on name input triggers doPromote',
  async ({ page }) => {
    await gotoAudit(page);
    await showPromoteBar(page);
    await waitForPromoteOptions(page);

    await page.locator('#promoteUserSelect').selectOption({ value: 'new' });
    const nameInput = page.locator('#promoteName');
    await expect(nameInput).toBeVisible();
    await expect(nameInput).toBeFocused();

    const testUser = `pw_test_${Date.now()}`;
    await nameInput.fill(testUser);
    await nameInput.press('Enter');

    const btn = page.locator('#promoteBtn');
    await expect(btn).toBeDisabled();
    const btnText = await btn.textContent();
    expect(btnText).toContain('Enrolling');

    await expect(page.locator('#promoteStatus')).toContainText('Success');
    await page.waitForTimeout(1_000);
    await expect(page.locator('#promoteBar')).toBeHidden();
  });

test('promote bar: clicking Enroll/Update with a selected user triggers fetch',
  async ({ page }) => {
    await gotoAudit(page);
    await showPromoteBar(page);
    await waitForPromoteOptions(page);

    await page.locator('#promoteUserSelect').selectOption({ label: 'Anthony' });

    const btn = page.locator('#promoteBtn');
    await btn.click();

    await expect(btn).toBeDisabled();
    const btnText = await btn.textContent();
    expect(btnText).toContain('Enrolling');

    await expect(page.locator('#promoteStatus')).toContainText('Success');
    await page.waitForTimeout(1_000);
    await expect(page.locator('#promoteBar')).toBeHidden();
  });

test('promote bar: empty name with "New User" selected does not submit',
  async ({ page }) => {
    await gotoAudit(page);
    await showPromoteBar(page);
    await waitForPromoteOptions(page);

    const sel = page.locator('#promoteUserSelect');
    await sel.selectOption({ value: 'new' });
    const nameInput = page.locator('#promoteName');
    await expect(nameInput).toBeVisible();

    await page.locator('#promoteBtn').click();
    await expect(nameInput).toBeFocused();
    await expect(page.locator('#promoteBtn')).not.toBeDisabled();
  });

test('promote bar: click same entry thumbnail twice toggles selection off',
  async ({ page }) => {
    await gotoAudit(page);

    await page.locator('.entry-thumb').first().click();
    await expect(page.locator('#promoteBar')).toBeVisible();

    await page.locator('.entry-thumb').first().click();
    await expect(page.locator('#promoteBar')).toBeHidden();
  });

test('promote bar: Escape key deselects entry',
  async ({ page }) => {
    await gotoAudit(page);

    await page.locator('.entry-thumb').first().click();
    await expect(page.locator('#promoteBar')).toBeVisible();

    await page.keyboard.press('Escape');
    await expect(page.locator('#promoteBar')).toBeHidden();
  });
