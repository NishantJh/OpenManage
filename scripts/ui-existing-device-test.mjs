// Browser check: a device already in the inventory without an OpenManage key is set up with
// the same one-step wizard (RADIUS tick box included), and a device that has a key keeps the
// replace-key form.
// It also checks that each screen showing a new RADIUS secret gives the AP commands, and
// writes the last shown commands as JSON to OPENMANAGE_UI_COMMANDS_OUT for the lab to run.
// Usage: OPENMANAGE_TEST_ROOT_PASSWORD=... node scripts/ui-existing-device-test.mjs <base-url> <username> <password> <device-ip>
// Needs the `playwright` package (NODE_PATH or cwd) and a Chromium (OPENMANAGE_UI_CHROMIUM).
import { createRequire } from "node:module";
import assert from "node:assert/strict";
import { writeFileSync } from "node:fs";

const [baseUrl, username, password, deviceIp] = process.argv.slice(2);
const rootPassword = process.env.OPENMANAGE_TEST_ROOT_PASSWORD;
if (!rootPassword) {
  console.error("usage: OPENMANAGE_TEST_ROOT_PASSWORD=... ui-existing-device-test.mjs <base-url> <username> <password> <device-ip>");
  process.exit(2);
}
const { chromium } = createRequire(process.cwd() + "/")("playwright");
const browser = await chromium.launch({ executablePath: process.env.OPENMANAGE_UI_CHROMIUM || undefined });
const errors = [];
const name = "lab-existing";
const lanAddress = "192.168.1.10";
// Optional screenshots of the key screens, for review.
const shot = (page, file) => process.env.OPENMANAGE_UI_SCREENSHOT_DIR ? page.screenshot({ path: process.env.OPENMANAGE_UI_SCREENSHOT_DIR + "/" + file, fullPage: true }) : null;

// readSteps checks the RADIUS steps on screen and returns the commands shown.
// The first time, it types the OpenManage address and checks it is saved; after that the
// address must already be filled in.
async function readSteps(page, { first = false } = {}) {
  const secret = await page.getByLabel("RADIUS shared secret").inputValue();
  assert.ok(secret.length >= 16, "RADIUS secret missing or too short");
  const addressField = page.getByLabel("OpenManage LAN address");
  if (first) {
    await addressField.fill(lanAddress);
    await addressField.blur();
    await page.getByText("Saved. Every device's steps use this address", { exact: false }).waitFor();
  } else {
    await page.getByText("Saved once for all devices.").waitFor();
    assert.equal(await addressField.inputValue(), lanAddress, "saved OpenManage address not filled in");
  }
  await page.getByLabel("Wi-Fi networks to change").fill("Home");
  const text = label => page.getByLabel(label, { exact: true }).innerText();
  const out = { secret, check: await text("Check command"), ssh: await text("SSH command"), backup: await text("Backup command"), setup: await text("AP setup commands"), undo: await text("Undo commands") };
  assert.equal(out.ssh, "ssh root@" + deviceIp);
  for (const want of ["auth_secret='" + secret + "'", "auth_server='" + lanAddress + "'", "auth_port='1812'", "encryption='wpa2'", "in 'Home')", "uci commit wireless"]) {
    assert.ok(out.setup.includes(want), "setup commands lack " + want + ":\n" + out.setup);
  }
  return out;
}
try {
  const page = await browser.newPage();
  page.on("pageerror", e => errors.push(e.message));
  await page.goto(baseUrl + "/");
  await page.getByLabel(/username/i).fill(username);
  await page.getByLabel(/password/i).fill(password);
  await page.getByRole("button", { name: /sign in|log in/i }).click();
  await page.locator("aside nav a", { hasText: "Devices" }).click();

  // Add device opens the wizard; the metadata-only form is behind Advanced / manual.
  await page.getByRole("button", { name: /add device/i }).first().click();
  await page.getByRole("heading", { name: "Add an access point" }).waitFor();
  await page.getByRole("button", { name: "Advanced / manual" }).click();
  await page.getByText("This records the device only", { exact: false }).waitFor();
  await page.getByLabel(/device name/i).fill(name);
  await page.getByLabel(/IP address or hostname/i).fill(deviceIp);
  await page.getByLabel(/Also let this device check Wi-Fi sign-ins/i).check();
  await shot(page, "1-manual-form.png");
  await page.locator("form").getByRole("button", { name: /add device/i }).click();
  await page.getByRole("heading", { name: "RADIUS secret for " + name }).waitFor();
  await readSteps(page, { first: true });
  await page.getByRole("button", { name: /I have saved it/i }).click();
  await page.locator("table td", { hasText: name }).first().waitFor();
  console.log("PASS: the Advanced / manual form has the RADIUS tick box and shows the secret with the AP steps");

  // The key icon on that device opens the wizard for it, with the RADIUS tick box.
  await page.getByRole("button", { name: "Enroll SSH credentials for " + name }).click();
  await page.getByRole("heading", { name: "Set up " + name }).waitFor();
  assert.equal(await page.getByLabel(/device ip address/i).inputValue(), deviceIp);
  assert.ok(await page.getByLabel(/device ip address/i).getAttribute("readonly") !== null, "address is editable");
  await page.getByRole("button", { name: /find device/i }).click();
  await page.getByText(/^SHA256:/).waitFor();
  const radius = page.getByLabel(/Also let this device check Wi-Fi sign-ins/i);
  await shot(page, "2-key-icon-wizard.png");
  assert.ok(await radius.isEnabled(), "RADIUS tick box disabled although the listener is on");
  // The manual form already created a secret, so the wizard warns that ticking replaces it.
  await page.getByText("already has a RADIUS secret", { exact: false }).waitFor();
  console.log("PASS: the key icon on an inventory-only device opens the wizard with the RADIUS tick box");
  await page.getByLabel(/I trust this network/i).check();
  await page.locator('input[type="password"]').fill(rootPassword);
  await radius.check();
  await page.getByRole("button", { name: /add and connect/i }).click();
  await page.getByLabel("RADIUS shared secret").waitFor({ timeout: 30000 });
  const steps = await readSteps(page);
  await page.locator("pre").first().scrollIntoViewIfNeeded();
  await shot(page, "3-radius-steps.png");
  if (process.env.OPENMANAGE_UI_COMMANDS_OUT) writeFileSync(process.env.OPENMANAGE_UI_COMMANDS_OUT, JSON.stringify(steps));
  console.log("PASS: the wizard shows the secret with the AP steps, pre-filled");
  await page.getByRole("button", { name: /I have saved it/i }).click();
  await page.getByText("Set up " + name + ". Key installed", { exact: false }).waitFor({ timeout: 30000 });
  await page.locator(".health-modal").getByRole("button", { name: "Close" }).click();
  assert.equal(await page.locator("table tr", { hasText: name }).count(), 1, "device row duplicated");
  console.log("PASS: the existing device is set up in place, with a RADIUS secret, and not duplicated");
  // Add device afterwards starts a fresh wizard, not one for the device just set up.
  await page.getByRole("button", { name: /add device/i }).first().click();
  await page.getByRole("heading", { name: "Add an access point" }).waitFor();
  assert.equal(await page.getByLabel(/device ip address/i).inputValue(), "", "fresh wizard kept the previous device");
  await page.locator(".modal .close").click();
  console.log("PASS: Add device after a device setup opens a fresh wizard");

  // Now that it has a key, the same icon offers the replace-key form, not the wizard.
  await page.getByRole("button", { name: "Enroll SSH credentials for " + name }).click();
  await page.getByText("SECURE SSH ENROLLMENT").waitFor();
  await page.getByRole("button", { name: "Cancel" }).click();
  console.log("PASS: a device that already has a key gets the replace-key form");

  await page.locator("aside nav a", { hasText: "Configuration" }).click();
  await page.locator("table td", { hasText: name }).first().waitFor();
  console.log("PASS: the device appears with a RADIUS secret on the Wi-Fi sign-in page");
  assert.deepEqual(errors, [], "page errors: " + errors.join("; "));
} finally {
  await browser.close();
}
