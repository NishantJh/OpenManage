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
  { label: "Device settings", path: "/settings", h1: "Device settings", has: ["Device settings"], absent: ["Device inventory", "Monitoring history"] },
  { label: "Floor plan", path: "/floor-plan", h1: "Floor plan", has: [], absent: ["Device inventory", "Monitoring history"] },
  { label: "Configuration", path: "/configuration", h1: "Configuration", has: [], absent: ["Device inventory", "Monitoring history"] },
  { label: "Monitoring", path: "/monitoring", h1: "Monitoring", has: ["Monitoring history"], absent: ["Device inventory"] },
  { label: "Access & security", path: "/access", h1: "Access & security", has: [], absent: ["Device inventory", "Monitoring history"] },
  { label: "Backups", path: "/backups", h1: "Backups", has: [], absent: ["Device inventory", "Monitoring history"] },
  { label: "Firmware", path: "/firmware", h1: "Firmware", has: ["Firmware and packages"], absent: ["Device inventory", "Monitoring history"] },
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
  await check(pages[pages.length - 2]);
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
  // Resize steps are a quarter foot, not a whole foot; a plain drag lands on a quarter, Shift drags freely.
  const handle = page.locator("rect.fp-handle").first();
  const dragHandle = async (dx, dy, shift) => {
    const hb = await handle.boundingBox();
    await page.mouse.move(hb.x + hb.width / 2, hb.y + hb.height / 2);
    if (shift) await page.keyboard.down("Shift");
    await page.mouse.down();
    await page.mouse.move(hb.x + hb.width / 2 + dx, hb.y + hb.height / 2 + dy, { steps: 6 });
    await page.mouse.up();
    if (shift) await page.keyboard.up("Shift");
    return page.locator("rect.fp-room").first().evaluate(e => Number(e.getAttribute("width")) / 0.3048);
  };
  const quarter = await dragHandle(37, 0, false);
  assert.ok(Math.abs(quarter * 4 - Math.round(quarter * 4)) < 1e-6 && Math.abs(quarter - Math.round(quarter)) > 0.1, `plain drag should land on a quarter foot, got ${quarter} ft`);
  const free = await dragHandle(3, 0, true);
  assert.ok(Math.abs(free * 4 - Math.round(free * 4)) > 0.01, `Shift drag should not snap, got ${free} ft`);
  await page.getByLabel(/width/i).first().fill("12");
  await page.locator(".fp-room-size", { hasText: "12 × 10 ft" }).waitFor();
  // A second room is placed beside the first, never on top of it.
  await page.getByRole("button", { name: /add room/i }).click();
  await page.waitForFunction(() => document.querySelectorAll("rect.fp-room").length === 2);
  const rects = await page.locator("rect.fp-room").evaluateAll(els => els.map(e => ["x", "y", "width", "height"].map(k => Number(e.getAttribute(k)))));
  const [[ax, ay, aw, ah], [bx, by, bw, bh]] = rects;
  assert.ok(!(ax < bx + bw - 0.01 && ax + aw > bx + 0.01 && ay < by + bh - 0.01 && ay + ah > by + 0.01), "new room overlaps the first");
  await page.locator(".fp-edit-row button[aria-label^='Remove']").last().click();
  await page.locator(".fp-room-name").first().click();
  await page.waitForFunction(() => document.activeElement?.getAttribute("data-room"), null, { timeout: 5000 });
  await page.keyboard.type("Kitchen");
  await page.locator(".fp-room-name", { hasText: "KITCHEN" }).waitFor();
  console.log("PASS: floor plan rooms in feet, renamable, canvas fits rooms");
  await page.locator(".fp-edit-row button[aria-label^='Remove']").first().click(); // rooms would hold the corner; start the image test without any
  // Plan image: upload a PNG (it lands on the top-left corner), scale it by typing a width in feet,
  // crop it to its top-left quarter, then remove it again.
  const png = Buffer.from("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGP4z8DwHwAFAAH/q842iQAAAABJRU5ErkJggg==", "base64");
  await page.locator('input[aria-label="Upload plan image"]').setInputFiles({ name: "plan.png", mimeType: "image/png", buffer: png });
  const planImg = page.locator("svg.fp-plan-image");
  await planImg.waitFor();
  const geom = () => planImg.evaluate(e => ["x", "y", "width", "height"].map(k => Number(e.getAttribute(k))));
  const [ix, iy] = await geom();
  assert.ok(ix === 0 && iy === 0, `plan image should sit on the top-left corner, got ${ix},${iy}`);
  await page.getByLabel("Plan image width").fill("30");
  await page.getByLabel("Plan image width").press("Enter");
  await page.waitForFunction(() => Math.abs(Number(document.querySelector("svg.fp-plan-image").getAttribute("width")) - 30 * 0.3048) < 0.01);
  // Dragging the picture away from the corner and saving: the next load pulls it back (snap on load).
  const ib = await planImg.boundingBox();
  await page.mouse.move(ib.x + ib.width / 2, ib.y + ib.height / 2);
  await page.mouse.down();
  await page.mouse.move(ib.x + ib.width / 2 + 120, ib.y + ib.height / 2 + 80, { steps: 5 });
  await page.mouse.up();
  const [mx] = await geom();
  assert.ok(mx > 1, `dragging should move the picture off the corner, got x=${mx}`);
  await page.getByRole("button", { name: /^save/i }).first().click();
  await page.getByText("Floor plan saved.").waitFor();
  await page.reload();
  await planImg.waitFor();
  const [sx, sy] = await geom();
  assert.ok(sx === 0 && sy === 0, `plan image should snap back to the corner on load, got ${sx},${sy}`);
  await page.getByRole("button", { name: "Crop" }).click();
  const [tl, br] = await page.locator("rect.fp-handle.img").all();
  const a = await tl.boundingBox(), z = await br.boundingBox();
  await page.mouse.move(z.x + z.width / 2, z.y + z.height / 2);
  await page.mouse.down();
  await page.mouse.move((a.x + z.x) / 2 + z.width / 2, (a.y + z.y) / 2 + z.height / 2, { steps: 5 });
  await page.mouse.up();
  await page.getByRole("button", { name: "Apply crop" }).click();
  await planImg.waitFor();
  const [cx, cy, cw, ch] = await geom();
  assert.ok(cx === 0 && cy === 0, `cropped image should stay on the corner, got ${cx},${cy}`);
  assert.ok(Math.abs(cw - 15 * 0.3048) < 0.3 && Math.abs(ch - cw) < 0.01, `crop should keep about half the width, got ${cw}x${ch}`);
  page.once("dialog", d => d.accept());
  await page.getByRole("button", { name: "Remove plan image" }).click();
  await planImg.waitFor({ state: "detached" });
  // The scale bar stays after the picture is gone, and a new upload reuses the width that was set.
  assert.match(await page.locator("svg.fp-svg g.fp-scale text").textContent(), /^\d+ ft$/, "scale bar should show after the image is removed");
  await page.locator('input[aria-label="Upload plan image"]').setInputFiles({ name: "plan.png", mimeType: "image/png", buffer: png });
  await planImg.waitFor();
  const [, , rw] = await geom();
  assert.ok(Math.abs(rw - cw) < 0.01, `re-upload should reuse the removed width ${cw}, got ${rw}`);
  page.once("dialog", d => d.accept());
  await page.getByRole("button", { name: "Remove plan image" }).click();
  await planImg.waitFor({ state: "detached" });
  console.log("PASS: floor plan image upload on the corner, scale, crop and removal");
  // Client dots follow the room of their AP. An AP on the wall shared by two rooms serves both, so its
  // clients are spread over both rooms rather than all landing in the first (API answers are mocked).
  const mocked = page;
  const json = body => ({ status: 200, contentType: "application/json", body: JSON.stringify(body) });
  const dev = (id, name) => ({ id, name, address: "192.0.2." + id, model: "", firmware: "", status: "online", last_seen_at: new Date().toISOString(), enrollable: false, config_drift: false, role: "access_point", dhcp_host: false, has_key: true });
  const clients = [];
  for (let i = 0; i < 8; i++) clients.push({ id: "c" + i, label: "", name: "Phone " + i, mac_hint: "aa:0" + i, hidden: false, device_id: i < 4 ? "1" : "2", interface: "wlan0", signal_dbm: -50, connected: true, last_seen: new Date().toISOString() });
  await mocked.route("**/api/devices", r => r.fulfill(json([dev("1", "AP-One"), dev("2", "AP-Two")])));
  await mocked.route("**/api/floor-plan", r => r.fulfill(json({
    rooms: [{ id: "a", name: "Living", x: 0, y: 0, w: 6, h: 5 }, { id: "b", name: "Office", x: 6, y: 0, w: 4, h: 5 }],
    placements: [{ device_id: "1", x: 3, y: 0 }, { device_id: "2", x: 6, y: 2.5 }] })));
  await mocked.route("**/api/wifi-clients", r => r.fulfill(json(clients)));
  await mocked.route("**/api/wifi-client-events**", r => r.fulfill(json([])));
  await mocked.goto(baseUrl + "/floor-plan");
  await mocked.locator("circle.fp-client").first().waitFor();
  const perRoom = await mocked.evaluate(() => {
    const rooms = [...document.querySelectorAll("rect.fp-room")].map(r => ["x", "y", "width", "height"].map(k => Number(r.getAttribute(k))));
    const n = [0, 0];
    document.querySelectorAll("circle.fp-client").forEach(c => { const x = Number(c.getAttribute("cx")), y = Number(c.getAttribute("cy")); rooms.forEach(([rx, ry, rw, rh], i) => { if (x >= rx && x <= rx + rw && y >= ry && y <= ry + rh) n[i]++; }); });
    return n;
  });
  assert.ok(perRoom[0] >= 4 && perRoom[1] >= 1, `clients of AP-One stay in Living and the shared-wall AP-Two reaches Office, got ${perRoom}`);
  // Moving an AP into a room next to the shared wall puts it inside that room, and its clients follow it.
  const toScreen = (px, py) => mocked.evaluate(([x, y]) => {
    const svg = document.querySelector("svg.fp-svg"), r = svg.getBoundingClientRect(), vb = svg.viewBox.baseVal;
    return { x: r.left + (x - vb.x) / vb.width * r.width, y: r.top + (y - vb.y) / vb.height * r.height };
  }, [px, py]);
  const from = await toScreen(6, 2.5), to = await toScreen(6.4, 2.5);
  await mocked.mouse.move(from.x, from.y);
  await mocked.mouse.down();
  await mocked.mouse.move(to.x, to.y, { steps: 6 });
  await mocked.mouse.up();
  await mocked.waitForFunction(() => {
    const rooms = [...document.querySelectorAll("rect.fp-room")].map(r => ["x", "y", "width", "height"].map(k => Number(r.getAttribute(k))));
    const n = [0, 0];
    document.querySelectorAll("circle.fp-client").forEach(c => { const x = Number(c.getAttribute("cx")), y = Number(c.getAttribute("cy")); rooms.forEach(([rx, ry, rw, rh], i) => { if (x >= rx && x <= rx + rw && y >= ry && y <= ry + rh) n[i]++; }); });
    return n[0] === 4 && n[1] === 4;
  }, null, { timeout: 5000 });
  await mocked.unrouteAll({ behavior: "ignoreErrors" });
  console.log("PASS: client dots follow their AP into its room, shared-wall AP spreads over both rooms");
  // An AP whose Wi-Fi poll failed is named in the Devices panel instead of silently having no devices
  // (API answers are mocked).
  const mockJson = body => ({ status: 200, contentType: "application/json", body: JSON.stringify(body) });
  const apDev = (id, name) => ({ id, name, address: "192.0.2." + id, model: "", firmware: "", status: "online", last_seen_at: new Date().toISOString(), enrollable: false, config_drift: false, role: "access_point", dhcp_host: false, has_key: true });
  await page.route("**/api/devices", r => r.fulfill(mockJson([apDev("1", "AP-One"), apDev("2", "AP-Two")])));
  await page.route("**/api/monitoring/settings", r => r.fulfill(mockJson({ enabled: true, interval_seconds: 300, client_tracking_enabled: true, client_poll_interval_seconds: 300 })));
  await page.route("**/api/wifi-clients/status", r => r.fulfill(mockJson([
    { device_id: "1", polled_at: new Date().toISOString(), ok: true, clients: 4 },
    { device_id: "2", polled_at: new Date().toISOString(), ok: false, error: "poll failed: device output exceeded the allowed limit", clients: 0 }])));
  await page.goto(baseUrl + "/floor-plan");
  const problems = page.locator('ul[aria-label="Access points that could not be read"]');
  await problems.waitFor();
  assert.match(await problems.innerText(), /AP-Two could not be read: poll failed: device output exceeded/);
  assert.ok(!/AP-One/.test(await problems.innerText()), "a healthy AP is not listed");
  await page.unrouteAll({ behavior: "ignoreErrors" });
  console.log("PASS: an AP whose Wi-Fi poll failed is shown in the Devices panel");
  assert.deepEqual(errors, [], "uncaught page errors");
  console.log("PASS: back button, deep link, no page errors");
} finally {
  await browser.close();
}
