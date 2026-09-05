// Manual verification script: mobile viewport → login → check composer chips,
// open the model drawer, screenshot each state.
// Needs: npm i --no-save playwright-core, a cached chromium in ~/.cache/ms-playwright,
// and the test server running (see repo root: ./chattoneko -config ... on :8123).
// Run: node test-mobile-picker.mjs
import { chromium } from 'playwright-core';
import { readdirSync, existsSync } from 'node:fs';
import { homedir } from 'node:os';
import { join } from 'node:path';

function findChromium() {
  const base = join(homedir(), '.cache', 'ms-playwright');
  if (!existsSync(base)) return undefined;
  for (const dir of readdirSync(base).sort().reverse()) {
    if (!dir.startsWith('chromium-')) continue;
    for (const sub of ['chrome-linux64', 'chrome-linux']) {
      const exe = join(base, dir, sub, 'chrome');
      if (existsSync(exe)) return exe;
    }
  }
  return undefined;
}

const browser = await chromium.launch({ executablePath: findChromium() });
const ctx = await browser.newContext({ viewport: { width: 390, height: 844 } });
const page = await ctx.newPage();
const errors = [];
page.on('pageerror', (e) => errors.push(String(e)));

await page.goto('http://127.0.0.1:8123');
await page.waitForTimeout(800);

// Login (two-phase: connect to server URL, then credentials)
if (await page.locator('#login-username').isVisible().catch(() => false)) {
  await page.locator('#login-username').fill('admin');
  await page.locator('#login-password').fill('cb951303');
  await page.locator('button[type="submit"]').click();
} else {
  await page.locator('button[type="submit"]').click(); // Connect first
  await page.waitForTimeout(800);
  await page.locator('#login-username').fill('admin');
  await page.locator('#login-password').fill('cb951303');
  await page.locator('button[type="submit"]').click();
}
await page.waitForTimeout(1500);

// 1. Composer bar: single centered borderless chip, selects hidden
const mobileRow = page.locator('div.sm\\:hidden').first();
const desktopRow = page.locator('div.sm\\:flex').first();
console.log('mobile chip row visible:', await mobileRow.isVisible());
console.log('desktop select row visible:', await desktopRow.isVisible());
console.log('mobile chip count:', await mobileRow.locator('button').count());
{
  const chipBox = await mobileRow.locator('button').first().boundingBox();
  const rowBox = await mobileRow.boundingBox();
  const off = chipBox && rowBox ? Math.abs((chipBox.x + chipBox.width / 2) - (rowBox.x + rowBox.width / 2)) : 999;
  console.log('chip centered (offset px):', Math.round(off * 10) / 10);
  console.log('chip border width:', await mobileRow.locator('button').first().evaluate((el) => getComputedStyle(el).borderTopWidth));
}
await page.screenshot({ path: '/tmp/mobile-composer.png' });

// 2. Open drawer via model chip
const chip = mobileRow.locator('button').first();
await chip.click();
await page.waitForTimeout(600);
console.log('drawer visible:', await page.locator('[data-slot="drawer-content"]').isVisible());
console.log('drawer descriptions:', await page.locator('[data-slot="drawer-description"]').count());
console.log('effort segments:', await page.locator('[data-slot="toggle-group-item"]').count());
console.log('model rows:', await page.locator('[data-slot="drawer-content"] button:has-text("/")').count());
await page.screenshot({ path: '/tmp/mobile-drawer.png' });

// 3. Select a model row → drawer STAYS open, chip updates
const modelRow = page.locator('[data-slot="drawer-content"] button:has-text("qwen/qwen3.8-max")').first();
await modelRow.click();
await page.waitForTimeout(400);
console.log('drawer open after model pick:', await page.locator('[data-slot="drawer-content"]').isVisible());
console.log('model chip now:', JSON.stringify((await chip.innerText()).trim()));

// 4. Tap an effort segment → applies AND closes the drawer
const seg = page.locator('[data-slot="toggle-group-item"]').first();
const segValue = (await seg.innerText()).trim();
await seg.click();
await page.waitForTimeout(700);
console.log('drawer closed after effort pick:', !(await page.locator('[data-slot="drawer-content"]').isVisible().catch(() => false)));
const chipText = (await chip.innerText()).trim().replace(/\s+/g, ' ');
console.log('chip shows model + effort:', JSON.stringify(chipText), '| has effort:', chipText.includes('-'));
await page.screenshot({ path: '/tmp/mobile-after-pick.png' });

// 5. Reopen: layout checks + re-tap active segment must not deselect/close
await chip.click();
await page.waitForTimeout(600);
// sr-only title: 1x1 clipped box (a11y present, visually hidden)
const titleBox = await page.locator('[data-slot="drawer-title"]').evaluate((el) => {
  const s = getComputedStyle(el);
  return { w: s.width, h: s.height, pos: s.position };
});
console.log('title sr-only (1x1 abs):', titleBox.w === '1px' && titleBox.h === '1px' && titleBox.pos === 'absolute');
// Compare TEXT left edges (range rect), not the full-width label divs
const textLeft = (sel) => page.locator(sel).evaluate((el) => {
  const r = document.createRange();
  r.selectNodeContents(el);
  return Math.round(r.getBoundingClientRect().x * 10) / 10;
});
const modelX = await textLeft('[data-slot="drawer-content"] div:text-is("Model")');
const effortX = await textLeft('[data-slot="drawer-content"] div:text-is("Reasoning effort")');
const rowX = await textLeft('[data-slot="drawer-content"] button span.font-mono >> nth=0');
const modelLabelBox = await page.locator('[data-slot="drawer-content"] div:text-is("Model")').boundingBox();
const effortLabelBox = await page.locator('[data-slot="drawer-content"] div:text-is("Reasoning effort")').boundingBox();
console.log('model above effort:', modelLabelBox && effortLabelBox ? modelLabelBox.y < effortLabelBox.y : 'n/a');
console.log('text lefts (model label / row / effort label):', modelX, rowX, effortX,
  '| aligned:', Math.abs(modelX - effortX) < 1 && Math.abs(modelX - rowX) < 1);
const segNow = page.locator('[data-slot="toggle-group-item"]').first();
await segNow.click(); // re-tap ACTIVE segment — must NOT deselect or close
await page.waitForTimeout(400);
console.log('re-tap active → still selected:', (await segNow.getAttribute('data-state')) === 'on',
  '| drawer open:', await page.locator('[data-slot="drawer-content"]').isVisible());
await page.screenshot({ path: '/tmp/mobile-drawer.png' });

// 5. Desktop check: same session, wider viewport → selects visible, chips hidden
if (await page.locator('[data-slot="drawer-content"]').isVisible().catch(() => false)) {
  await page.mouse.click(195, 200); // overlay click closes the drawer
  await page.waitForTimeout(600);
}
await page.setViewportSize({ width: 1280, height: 900 });
await page.waitForTimeout(500);
console.log('desktop: select row visible:', await page.locator('div.sm\\:flex').first().isVisible());
console.log('desktop: chip row visible:', await page.locator('div.sm\\:hidden').first().isVisible());
await page.screenshot({ path: '/tmp/desktop-composer.png' });

console.log('page errors:', errors.length ? errors : 'none');
await browser.close();
