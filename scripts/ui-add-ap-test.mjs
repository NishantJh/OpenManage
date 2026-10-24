// Browser check for the "Add an access point" wizard against a running OpenManage.
// Usage: OPENMANAGE_TEST_ROOT_PASSWORD=... node scripts/ui-add-ap-test.mjs <base-url> <username> <password> <device-ip>
// The router root password comes from the environment so it never appears in the process list.
// Needs the `playwright` package (NODE_PATH or cwd) and a Chromium (OPENMANAGE_UI_CHROMIUM).
import { createRequire } from "node:module";
import assert from "node:assert/strict";

const [baseUrl, username, password, deviceIp] = process.argv.slice(2);
const rootPassword = process.env.OPENMANAGE_TEST_ROOT_PASSWORD;
if (!rootPassword) {
  console.error("usage: OPENMANAGE_TEST_ROOT_PASSWORD=... ui-add-ap-test.mjs <base-url> <username> <password> <device-ip>");
  process.exit(2);
}
const { chromium } = createRequire(process.cwd() + "/")("playwright");
const browser = await chromium.launch({ executablePath: process.env.OPENMANAGE_UI_CHROMIUM || undefined });
const errors = [];
try {
  const page = await browser.newPage();
  page.on("pageerror", e => errors.push(e.message));
  const requests = [];
  page.on("request", r => { if (r.method() === "POST") requests.push(r.postData() || ""); });
  await page.goto(baseUrl + "/");
  await page.getByLabel(/username/i).fill(username);
  await page.getByLabel(/password/i).fill(password);
  await page.getByRole("button", { name: /sign in|log in/i }).click();
  await page.locator("aside nav a", { hasText: "Devices" }).click();
  await page.getByRole("button", { name: /add device/i }).first().click();

  // The never-stored notice is on screen before any password field exists.
  await page.getByText("used once and never stored", { exact: false }).waitFor();
  assert.equal(await page.locator('input[type="password"]').count(), 0, "password field shown before the notice step");
  console.log("PASS: wizard shows the password notice before asking for it");

  await page.getByLabel(/device ip address/i).fill(deviceIp);
  await page.getByRole("button", { name: /find device/i }).click();
  await page.getByText(/^SHA256:/).waitFor();
  console.log("PASS: wizard shows the router host-key fingerprint to confirm");

  const add = page.getByRole("button", { name: /add and connect/i });
  assert.ok(await add.isDisabled(), "add enabled before the trust confirmation");
  await page.getByLabel(/I trust this network/i).check();
  await page.locator('input[type="password"]').fill("definitely-wrong-password");
  await page.getByRole("button", { name: /add and connect/i }).click();
  await page.getByText("rejected the username or password").waitFor();
  await page.getByText("password you use to sign in to LuCI").waitFor();
  console.log("PASS: wrong password shows a specific, actionable error");

  await page.locator('input[type="password"]').fill(rootPassword);
  await page.getByRole("button", { name: /add and connect/i }).click();
  await page.getByText("Key installed and connection verified", { exact: false }).waitFor({ timeout: 30000 });
  await page.locator("table td", { hasText: deviceIp }).first().waitFor();
  console.log("PASS: device added through the wizard and shown in the inventory");

  // The password is posted once to the bootstrap endpoint and is not kept in the page.
  assert.ok(await page.locator('input[type="password"]').count() === 0, "wizard still open");
  assert.ok(!(await page.locator("input,textarea").evaluateAll(els => els.map(e => e.value))).includes(rootPassword), "root password left in a form field");
  assert.ok(!(await page.locator("body").innerText()).includes(rootPassword) || rootPassword.length < 8, "root password shown on the page");
  console.log("PASS: root password is not left in the page");
  assert.deepEqual(errors, [], "page errors: " + errors.join("; "));
} finally {
  await browser.close();
}
