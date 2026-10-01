const { chromium } = require('playwright');
const assert = require('assert');

(async () => {
  console.log('Starting Playwright test...');
  const browser = await chromium.launch({ headless: true });
  const context = await browser.newContext();
  const page = await context.newPage();

  const baseUrl = 'http://localhost:8081';
  const testName = 'PromotedUserTest';

  try {
    console.log('Step 1: Navigating to Audit page...');
    await page.goto(`${baseUrl}/ui/audit`);
    
    // Wait for entries to load
    await page.waitForSelector('.entry', { timeout: 5000 });

    const entries = await page.locator('.entry').all();
    console.log(`Found ${entries.length} audit entries. Searching for one with a face...`);

    let targetEntry = null;
    let targetThumb = null;
    let entryName = '';

    for (const entry of entries) {
      // Check if it has a face thumbnail image
      const thumb = entry.locator('img.face-thumb');
      if (await thumb.count() > 0) {
        targetEntry = entry;
        targetThumb = thumb;
        const nameElement = entry.locator('div[style*="font-weight:600"]');
        entryName = (await nameElement.textContent()).trim();
        break;
      }
    }

    if (!targetEntry || !targetThumb) {
      throw new Error('No audit entries with faces found in the log.');
    }

    console.log(`Found entry for: ${entryName}. Proceeding to promote...`);

    // Step 2: Click the THUMBNAIL to show the promote bar
    await targetThumb.click();
    await page.waitForSelector('#promoteBar', { state: 'visible' });

    // Step 3: Enter new name and click Promote
    console.log(`Entering new name: ${testName}`);
    await page.fill('#promoteName', testName);
    await page.click('#promoteBtn');

    // Step 4: Wait for success message
    console.log('Waiting for promotion success message...');
    await page.waitForSelector('#promoteStatus:has-text("✓ Promoted!")', { timeout: 5000 });
    console.log('Promotion successful!');

    // Step 5: Verify on Enroll page
    console.log('Step 5: Verifying on Enroll page...');
    await page.goto(`${baseUrl}/ui/enroll`);
    
    // Wait for the user list to load (it's in the body)
    await page.waitForLoadState('networkidle');

    const content = await page.textContent('body');
    if (content.includes(testName)) {
      console.log(`SUCCESS: ${testName} was found in the enroll list!`);
    } else {
      throw new Error(`FAILURE: ${testName} was NOT found in the enroll list.`);
    }

  } catch (err) {
    console.error('TEST FAILED:', err.message);
    process.exit(1);
  } finally {
    await browser.close();
    console.log('Browser closed.');
  }
})();
