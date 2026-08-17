// Captures real, current screenshots of the plugin for the README /
// plugin.json's info.screenshots -- run against a real local Grafana
// instance with the plugin installed and a real LLM provider configured.
// Update BASE/credentials/dashboard title below to match your environment.
const puppeteer = require('puppeteer');
const path = require('path');

const BASE = process.env.SCREENSHOT_BASE_URL || 'http://localhost:3000';
const USER = process.env.SCREENSHOT_USER || 'admin';
const PASSWORD = process.env.SCREENSHOT_PASSWORD || 'admin';
const DASHBOARD_TITLE = process.env.SCREENSHOT_DASHBOARD || 'Capacity';
const DIR = path.join(__dirname, '..', 'docs', 'screenshots');

async function login(page) {
  await page.goto(`${BASE}/login`, { waitUntil: 'networkidle2' });
  await page.type('input[name="user"]', USER);
  await page.type('input[name="password"]', PASSWORD);
  await Promise.all([
    page.click('button[type="submit"]'),
    page.waitForResponse((r) => r.url().includes('/api/login') && r.status() === 200).catch(() => {}),
  ]);
  await new Promise((r) => setTimeout(r, 1500));
}

async function waitForAssistantReply(page, timeoutMs) {
  const start = Date.now();
  // Wait for the send button to be disabled (request in flight) then
  // re-enabled (response fully done) -- more reliable than matching on
  // emotion's hashed class names, which don't preserve component names in a
  // production build.
  let sawDisabled = false;
  while (Date.now() - start < timeoutMs) {
    const state = await page.evaluate(() => {
      const btn = document.querySelector('[data-testid="send-message-button"]');
      return btn ? btn.disabled : null;
    });
    if (state === true) {
      sawDisabled = true;
    }
    if (sawDisabled && state === false) {
      return true;
    }
    await new Promise((r) => setTimeout(r, 1000));
  }
  return false;
}

(async () => {
  const browser = await puppeteer.launch({ headless: true, args: ['--no-sandbox'] });
  const page = await browser.newPage();
  await page.setViewport({ width: 1600, height: 1000 });
  await login(page);

  console.log('1. Chat landing page...');
  await page.goto(`${BASE}/a/agent-ai-app/chat`, { waitUntil: 'networkidle2' });
  await new Promise((r) => setTimeout(r, 1500));
  await page.screenshot({ path: path.join(DIR, 'chat-landing.png') });

  console.log('2. Active conversation with a real response...');
  const quickPrompt = await page.$('[data-testid="quick-prompt-information"]');
  if (quickPrompt) {
    await quickPrompt.click();
  } else {
    const textarea = await page.$('textarea');
    if (textarea) {
      await textarea.type('What datasources are available?');
      await page.keyboard.press('Enter');
    }
  }
  await waitForAssistantReply(page, 60000);
  await new Promise((r) => setTimeout(r, 1000));
  await page.screenshot({ path: path.join(DIR, 'chat-conversation.png') });

  console.log('3. Agents admin page...');
  await page.goto(`${BASE}/a/agent-ai-app/agents`, { waitUntil: 'networkidle2' });
  await new Promise((r) => setTimeout(r, 1200));
  await page.screenshot({ path: path.join(DIR, 'agents-page.png') });

  console.log('4. Configuration page...');
  await page.goto(`${BASE}/plugins/agent-ai-app?page=configuration`, { waitUntil: 'networkidle2' });
  await new Promise((r) => setTimeout(r, 1200));
  await page.screenshot({ path: path.join(DIR, 'configuration-page.png'), fullPage: true });

  console.log('5. Dashboard Chat page...');
  await page.goto(`${BASE}/a/agent-ai-app/dashboard-chat`, { waitUntil: 'networkidle2' });
  await new Promise((r) => setTimeout(r, 1200));
  const dashboardPicker = await page.$('input[id*="react-select"]') || await page.$('input[class*="grafana-select"]');
  if (dashboardPicker) {
    await dashboardPicker.click();
    await page.keyboard.type(DASHBOARD_TITLE);
    await new Promise((r) => setTimeout(r, 800));
    await page.keyboard.press('Enter');
    await new Promise((r) => setTimeout(r, 2000));
  }
  await page.screenshot({ path: path.join(DIR, 'dashboard-chat.png') });

  console.log('6. Panel-menu "Agent AI" (explain_panel)...');
  await page.goto(`${BASE}/dashboards`, { waitUntil: 'networkidle2' });
  await new Promise((r) => setTimeout(r, 1000));
  const dashLink = await page.evaluateHandle(
    (title) => [...document.querySelectorAll('a')].find((a) => a.textContent?.trim() === title),
    DASHBOARD_TITLE
  );
  const dashLinkEl = dashLink.asElement();
  if (dashLinkEl) {
    await dashLinkEl.click();
    await page.waitForNavigation({ waitUntil: 'networkidle2' }).catch(() => {});
    await new Promise((r) => setTimeout(r, 2500));
    const panel = await page.$('[data-viz-panel-key]');
    if (panel) {
      await panel.hover();
      await new Promise((r) => setTimeout(r, 600));
      const menuBtn = await panel.$('button[aria-label*="Menu" i]');
      if (menuBtn) {
        await menuBtn.click();
        await new Promise((r) => setTimeout(r, 600));
        const items = await page.$$('[role="menuitem"]');
        for (const item of items) {
          const text = await item.evaluate((el) => el.textContent?.trim());
          if (text === 'Extensions') {
            await item.hover();
            await new Promise((r) => setTimeout(r, 800));
          }
        }
        const agentAiItem = await page.evaluateHandle(() =>
          [...document.querySelectorAll('[role="menuitem"]')].find((el) => el.textContent?.includes('Agent AI'))
        );
        const agentAiEl = agentAiItem.asElement();
        if (agentAiEl) {
          await agentAiEl.click();
          await waitForAssistantReply(page, 45000);
          await new Promise((r) => setTimeout(r, 1000));
        }
      }
    }
  }
  await page.screenshot({ path: path.join(DIR, 'panel-menu-explain.png') });

  await browser.close();
  console.log('\nDone. Screenshots saved to', DIR);
})().catch((e) => {
  console.error(e);
  process.exit(1);
});
