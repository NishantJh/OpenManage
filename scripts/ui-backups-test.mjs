// Browser check for the Backups page against a running OpenManage and an enrolled device.
// Usage: OPENMANAGE_TEST_MISMATCH_RELEASE=23.05.5 node scripts/ui-backups-test.mjs <base-url> <username> <password> <device-name>
// The device must already have a backup whose release is that mismatch value, so the restore
// dialog's second confirmation is exercised. The restore reboots the device; whoever runs this
// brings the device back (the lab script starts the container again).
// Needs the `playwright` package (NODE_PATH or cwd) and a Chromium (OPENMANAGE_UI_CHROMIUM).
import { createRequire } from "node:module";
import assert from "node:assert/strict";

const [baseUrl, username, password, deviceName] = process.argv.slice(2);
const mismatch = process.env.OPENMANAGE_TEST_MISMATCH_RELEASE;
if (!baseUrl || !username || !password || !deviceName || !mismatch) {
  console.error("usage: OPENMANAGE_TEST_MISMATCH_RELEASE=... ui-backups-test.mjs <base-url> <username> <password> <device-name>");
  process.exit(2);
}
const { chromium } = createRequire(process.cwd() + "/")("playwright");
const shots = process.env.OPENMANAGE_UI_SHOTS;
const browser = await chromium.launch({ executablePath: process.env.OPENMANAGE_UI_CHROMIUM || undefined });
const errors = [];
try {
  const page = await browser.newPage({ viewport: { width: 1280, height: 1000 } });
  page.on("pageerror", e => errors.push(e.message));
  const shot = async name => { if (shots) await page.screenshot({ path: `${shots}/${name}.png`, fullPage: true }); };
  await page.goto(baseUrl + "/");
  await page.getByLabel(/username/i).fill(username);
  await page.getByLabel(/password/i).fill(password);
  await page.getByRole("button", { name: /sign in|log in/i }).click();
  await page.locator("aside nav a", { hasText: "Backups" }).click();
  await page.getByRole("heading", { name: "Device backups" }).waitFor();
  await page.getByLabel("Device", { exact: true }).selectOption({ label: deviceName });
  console.log("PASS: the Backups page lists device backups for an enrolled device");

  // Back up now: the newest backup is the already-stored one, so an unchanged device stores nothing.
  const rows = () => page.locator(".device-backups tbody tr");
  const before = await rows().count();
  await page.getByRole("button", { name: /back up this device/i }).click();
  await page.locator(".backup-message").waitFor();
  const outcome = await page.locator(".backup-message").innerText();
  assert.match(outcome, /Backup stored|has not changed/, outcome);
  assert.ok(await rows().count() >= before, "backup list shrank");
  console.log("PASS: Back up this device reports what happened");
  await page.getByText(/Keeping \d+ of 5/).waitFor();
  console.log("PASS: the list says how many backups are kept of the limit");

  // Download needs the password again; a wrong one must not produce a file.
  await page.getByRole("button", { name: /^Download backup from/ }).first().click();
  await page.getByText("root password hash", { exact: false }).waitFor();
  await page.locator('input[type="password"]').fill("definitely-wrong");
  await page.getByRole("button", { name: /^download$/i }).click();
  await page.locator(".modal .error").waitFor();
  console.log("PASS: a wrong password refuses the download");
  await page.locator('input[type="password"]').fill(password);
  const [download] = await Promise.all([page.waitForEvent("download"), page.getByRole("button", { name: /^download$/i }).click()]);
  const chunks = [];
  for await (const c of await download.createReadStream()) chunks.push(c);
  const file = Buffer.concat(chunks);
  assert.ok(file.length > 100 && file[0] === 0x1f && file[1] === 0x8b, "download is not a gzip archive");
  assert.match(download.suggestedFilename(), /^openmanage-backup-.*\.tar\.gz$/);
  await page.getByText("Backup downloaded").waitFor();
  console.log("PASS: the right password downloads a gzip archive");

  // Retention is an administrator setting between 1 and 20.
  const retention = page.getByLabel("Backups kept per device");
  await retention.fill("3");
  await page.getByRole("button", { name: /save retention|save settings/i }).click();
  await page.getByText("Backup settings saved.").waitFor();
  await page.getByText(/Keeping \d+ of 3/).waitFor();
  console.log("PASS: changing the retention updates the limit shown");
  await retention.fill("99");
  await retention.blur();
  assert.equal(await retention.inputValue(), "20", "retention is not capped at 20");
  await retention.fill("5");
  await page.getByRole("button", { name: /save retention|save settings/i }).click();
  await page.getByText(/Keeping \d+ of 5/).waitFor();
  await shot("backups");

  // Restore: wrong or missing name keeps the button disabled; a release mismatch needs a second confirmation.
  await rows().filter({ hasText: mismatch }).getByRole("button", { name: /^Restore backup from/ }).click();
  await page.getByText("reboots", { exact: false }).first().waitFor();
  const restore = page.getByRole("button", { name: /restore and reboot/i });
  assert.ok(await restore.isDisabled(), "restore enabled before the name was typed");
  await page.locator(".restore-modal input").fill(deviceName + "x");
  assert.ok(await restore.isDisabled(), "restore enabled with a wrong name");
  await page.locator(".restore-modal input").fill(deviceName);
  assert.ok(await restore.isEnabled(), "restore disabled with the right name");
  await restore.click();
  await page.getByText("restoring across releases", { exact: false }).waitFor();
  assert.ok(await restore.isDisabled(), "restore enabled before the release mismatch was accepted");
  await shot("restore-dialog");
  await page.getByLabel(/I understand and want to restore anyway/).check();
  await restore.click();
  console.log("PASS: restore needs the device name and a second confirmation for a release mismatch");
  await page.getByText("Restore in progress").waitFor();
  await page.getByText("Restore in progress").waitFor({ state: "detached", timeout: 300000 });
  await page.locator(".restore-history tbody tr").first().getByText("RESTORED", { exact: true }).waitFor();
  console.log("PASS: the page follows the restore from running to restored");
  await shot("restored");

  // Delete one backup (not the one just restored from, so history stays meaningful).
  page.once("dialog", d => d.accept());
  await rows().first().getByRole("button", { name: /^Delete backup from/ }).click();
  await page.getByText("Backup deleted.").waitFor();
  console.log("PASS: an administrator can delete a backup");
  assert.deepEqual(errors, [], "page errors: " + errors.join("; "));
} catch (e) {
  const text = await browser.contexts()[0]?.pages()[0]?.locator(".device-backups").innerText().catch(() => "");
  console.error("Backups section at failure:\n" + text);
  throw e;
} finally {
  await browser.close();
}
