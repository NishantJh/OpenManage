// Browser check for sidebar navigation against a running OpenManage.
// Usage: node scripts/ui-nav-test.mjs <base-url> <username> <password>
// Needs the `playwright` package (resolved from NODE_PATH or cwd) and a Chromium.
// OPENMANAGE_UI_CHROMIUM may point at a Chromium binary.
import { createRequire } from "node:module";
import assert from "node:assert/strict";

const [baseUrl, username, password] = process.argv.slice(2);
if (!baseUrl || !username || !password) {
  console.error("usage: ui-nav-test.mjs <base-url> <username> <password>");
  process.exit(2);
}
const { chromium } = createRequire(process.cwd() + "/")("playwright");

// Each page: sidebar label, URL path, and an h2/h1 heading unique to that page.
// "absent" lists headings that belong to OTHER pages and must not render here.
const pages = [
  { label: "Overview", path: "/", h1: "Your network, at a glance.", has: [], absent: ["Device inventory", "Monitoring history", "Configuration snapshots"] },
  { label: "Devices", path: "/devices", h1: "Devices", has: ["Device inventory"], absent: ["Monitoring history"] },
  { label: "Floor plan", path: "/floor-plan", h1: "Floor plan", has: [], absent: ["Device inventory", "Monitoring history"] },
  { label: "Configuration", path: "/configuration", h1: "Configuration", has: [], absent: ["Device inventory", "Monitoring history"] },
  { label: "Monitoring", path: "/monitoring", h1: "Monitoring", has: ["Monitoring history"], absent: ["Device inventory"] },
  { label: "Access & security", path: "/access", h1: "Access & security", has: [], absent: ["Device inventory", "Monitoring history"] },
  { label: "Backups", path: "/backups", h1: "Backups", has: [], absent: ["Device inventory", "Monitoring history"] },
];

const browser = await chromium.launch({ executablePath: process.env.OPENMANAGE_UI_CHROMIUM || undefined });
const errors = [];
try {
  const page = await browser.newPage();
  page.on("pageerror", e => errors.push(e.message));
  await page.goto(baseUrl + "/");
  await page.getByLabel(/username/i).fill(username);
  await page.getByLabel(/password/i).fill(password);
  await page.getByRole("button", { name: /sign in|log in/i }).click();
  await page.locator("aside nav a", { hasText: "Overview" }).waitFor();

  const headings = async () => page.locator("h2").allInnerTexts();
  const check = async p => {
    assert.equal(new URL(page.url()).pathname, p.path, `${p.label}: URL`);
    await page.locator("h1", { hasText: p.h1 }).waitFor();
    assert.equal((await page.locator("aside nav a.active").allInnerTexts()).length, 1, `${p.label}: one active item`);
    assert.match(await page.locator("aside nav a.active").innerText(), new RegExp(p.label), `${p.label}: active item`);
    const hs = await headings();
    for (const h of p.has) assert.ok(hs.includes(h), `${p.label}: missing "${h}"`);
    for (const h of p.absent) assert.ok(!hs.includes(h), `${p.label}: "${h}" belongs to another page`);
    if (p.path === "/") await page.getByText("Needs attention").waitFor();
    console.log("PASS: sidebar → " + p.label);
  };

  for (const p of pages) {
    await page.locator("aside nav a", { hasText: p.label }).click();
    await check(p);
  }
  await page.goBack();
  await check(pages[5]);
  await page.goto(baseUrl + "/monitoring"); // deep link / reload must hit the SPA fallback
  await page.locator("aside nav a.active", { hasText: "Monitoring" }).waitFor();
  await check(pages[4]);
  // Phone width hides the sidebar; the page switcher must still reach every page.
  await page.setViewportSize({ width: 390, height: 800 });
  assert.ok(!(await page.locator("aside.sidebar").isVisible()), "sidebar hidden on mobile");
  for (const p of pages) {
    await page.locator(".mobile-nav a", { hasText: p.label }).click();
    await page.locator("h1", { hasText: p.h1 }).waitFor();
    assert.equal(new URL(page.url()).pathname, p.path, `mobile ${p.label}: URL`);
  }
  console.log("PASS: mobile page switcher reaches every page");
  // Floor plan rooms: sizes in feet, rename from the map, canvas follows the rooms (nothing is saved).
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.goto(baseUrl + "/floor-plan");
  await page.locator("h1", { hasText: "Floor plan" }).waitFor();
  const box = async () => (await page.locator("svg.fp-svg").getAttribute("viewBox")).split(" ").map(Number);
  await page.getByRole("button", { name: /add room/i }).click();
  await page.locator(".fp-room-size", { hasText: "12 × 10 ft" }).waitFor();
  const small = await box();
  await page.getByLabel(/width/i).first().fill("60");
  await page.locator(".fp-room-size", { hasText: "60 × 10 ft" }).waitFor();
  const wide = await box();
  assert.ok(wide[2] > small[2], "canvas grows with the room");
  await page.getByLabel(/width/i).first().fill("12");
  const back = await box();
  assert.ok(back[2] < wide[2], "canvas shrinks with the room");
  await page.locator(".fp-room-name").first().click();
  await page.waitForFunction(() => document.activeElement?.getAttribute("data-room"), null, { timeout: 5000 });
  await page.keyboard.type("Kitchen");
  await page.locator(".fp-room-name", { hasText: "KITCHEN" }).waitFor();
  console.log("PASS: floor plan rooms in feet, renamable, canvas fits rooms");
  assert.deepEqual(errors, [], "uncaught page errors");
  console.log("PASS: back button, deep link, no page errors");
} finally {
  await browser.close();
}
