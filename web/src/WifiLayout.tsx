import { useEffect, useMemo, useState } from "react";
import { Plus, Save, RefreshCw, RadioTower, Ruler, ScanLine, Trash2 } from "lucide-react";

type Device = { id: string; name: string; address: string; model: string; status: string };
type Room = { id: string; name: string; x: number; y: number; width: number; height: number };
type Wall = { id: string; x1: number; y1: number; x2: number; y2: number; material: string };
type Placement = { device_id: string; label: string; x: number; y: number };
type Layout = { width_m: number; height_m: number; rooms: Room[]; walls: Wall[]; aps: Placement[]; updated_at?: string };
type Observation = { id: number; source_device_id: string; source_name: string; source_radio: string; source_bssid: string; observed_bssid: string; ssid: string; channel: number; signal_dbm: number; observed_at: string };
const initial: Layout = { width_m: 12, height_m: 8, rooms: [], walls: [], aps: [] };
const uid = () => Math.random().toString(36).slice(2, 10);

function csrfToken() {
  return document.cookie.split("; ").find((c) => c.startsWith("om_csrf="))?.slice("om_csrf=".length) ?? "";
}

export default function WifiLayout({ devices, role, onNotice }: { devices: Device[]; role: string; onNotice: (message: string) => void }) {
  const [layout, setLayout] = useState<Layout>(initial);
  const [observations, setObservations] = useState<Observation[]>([]);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [scanning, setScanning] = useState(false);
  const [error, setError] = useState("");
  const [dragging, setDragging] = useState<string | null>(null);
  const [wallMode, setWallMode] = useState(false);
  const [wallStart, setWallStart] = useState<{ x: number; y: number } | null>(null);
  const [wallMaterial, setWallMaterial] = useState("brick");
  const deviceMap = useMemo(() => new Map(devices.map((d) => [d.id, d])), [devices]);

  async function load() {
    setLoading(true); setError("");
    try {
      const [layoutResponse, observationsResponse] = await Promise.all([
        fetch("/api/wifi/layout"), fetch("/api/wifi/observations")
      ]);
      if (!layoutResponse.ok) throw new Error("Could not load the saved layout.");
      const data = await layoutResponse.json() as Layout;
      setLayout({ ...initial, ...data, rooms: data.rooms ?? [], walls: data.walls ?? [], aps: data.aps ?? [] });
      if (observationsResponse.ok) setObservations(await observationsResponse.json() as Observation[]);
    } catch (e) {
      setError(e instanceof Error ? e.message : "Could not load Wi-Fi coverage data.");
    } finally { setLoading(false); }
  }
  useEffect(() => { void load(); }, []);

  function toPoint(e: React.PointerEvent<SVGSVGElement>) {
    const rect = e.currentTarget.getBoundingClientRect();
    const x = Math.max(0, Math.min(layout.width_m, (e.clientX - rect.left) / rect.width * layout.width_m));
    const y = Math.max(0, Math.min(layout.height_m, (e.clientY - rect.top) / rect.height * layout.height_m));
    return { x: Math.round(x * 10) / 10, y: Math.round(y * 10) / 10 };
  }
  function pointerMove(e: React.PointerEvent<SVGSVGElement>) {
    if (!dragging) return;
    const p = toPoint(e);
    setLayout((old) => ({ ...old, aps: old.aps.map((ap) => ap.device_id === dragging ? { ...ap, ...p } : ap) }));
  }
  function canvasClick(e: React.MouseEvent<SVGSVGElement>) {
    if (!wallMode) return;
    const rect = e.currentTarget.getBoundingClientRect();
    const p = { x: Math.round(Math.max(0, Math.min(layout.width_m, (e.clientX - rect.left) / rect.width * layout.width_m)) * 10) / 10,
      y: Math.round(Math.max(0, Math.min(layout.height_m, (e.clientY - rect.top) / rect.height * layout.height_m)) * 10) / 10 };
    if (!wallStart) setWallStart(p);
    else {
      if (Math.hypot(p.x - wallStart.x, p.y - wallStart.y) > 0.2) setLayout((old) => ({ ...old, walls: [...old.walls, { id: uid(), x1: wallStart.x, y1: wallStart.y, x2: p.x, y2: p.y, material: wallMaterial }] }));
      setWallStart(null); setWallMode(false);
    }
  }
  function addRoom() {
    const index = layout.rooms.length + 1;
    const x = Math.min(Math.max(0, layout.width_m - 3), (index - 1) % 3 * 3);
    const y = Math.min(Math.max(0, layout.height_m - 2.5), Math.floor((index - 1) / 3) * 2.5);
    setLayout((old) => ({ ...old, rooms: [...old.rooms, { id: uid(), name: "Room " + index, x, y, width: Math.min(3, old.width_m - x), height: Math.min(2.5, old.height_m - y) }] }));
  }
  function toggleAP(device: Device, enabled: boolean) {
    setLayout((old) => ({ ...old, aps: enabled
      ? [...old.aps, { device_id: device.id, label: device.name, x: old.width_m / 2, y: old.height_m / 2 }]
      : old.aps.filter((ap) => ap.device_id !== device.id) }));
  }
  async function save() {
    setSaving(true); setError("");
    try {
      const response = await fetch("/api/wifi/layout", { method: "PUT", headers: { "Content-Type": "application/json", "X-CSRF-Token": csrfToken() }, body: JSON.stringify({ ...layout, updated_at: undefined }) });
      const data = await response.json();
      if (!response.ok) throw new Error(data.error || "Could not save Wi-Fi layout.");
      setLayout({ ...initial, ...data, rooms: data.rooms ?? [], walls: data.walls ?? [], aps: data.aps ?? [] });
      onNotice("Wi-Fi layout saved to PostgreSQL.");
    } catch (e) { setError(e instanceof Error ? e.message : "Could not save Wi-Fi layout."); }
    finally { setSaving(false); }
  }
  async function scanAll() {
    setScanning(true); setError("");
    try {
      let scanned = 0; const failures: string[] = [];
      for (const ap of layout.aps) {
        const device = deviceMap.get(ap.device_id);
        if (!device) continue;
        try {
          const response = await fetch("/api/devices/" + encodeURIComponent(ap.device_id) + "/wifi-scan", { method: "POST", headers: { "X-CSRF-Token": csrfToken() } });
          const data = await response.json();
          if (!response.ok) throw new Error(data.error || "scan failed");
          scanned += Number(data.count) || 0;
        } catch (e) { failures.push(device.name + ": " + (e instanceof Error ? e.message : "scan failed")); }
      }
      const response = await fetch("/api/wifi/observations");
      if (response.ok) setObservations(await response.json() as Observation[]);
      if (failures.length) setError("Collected " + scanned + " observations. " + failures.join("; "));
      else onNotice("Collected " + scanned + " Wi-Fi RSSI observations across " + layout.aps.length + " access points.");
    } catch (e) { setError(e instanceof Error ? e.message : "Could not collect radio observations."); }
    finally { setScanning(false); }
  }

  const writable = role === "admin" || role === "operator";
  const recent = observations.slice(0, 120);
  const bssidOwners = useMemo(() => new Map(observations.filter((o) => o.source_bssid).map((o) => [o.source_bssid.toUpperCase(), o.source_name])), [observations]);
  return <section className="wifi-layout">
    <div className="wifi-layout-head"><div><div className="eyebrow">RADIO PLANNING / COVERAGE</div><h2>Wi-Fi layout & signal observations</h2><p>Map rooms and AP positions, then collect RSSI readings from each enrolled OpenWrt radio.</p></div>
      <div className="wifi-layout-actions"><button className="secondary" onClick={() => void load()} disabled={loading}><RefreshCw size={14}/> Reload</button>{writable && <button className="secondary" onClick={() => void save()} disabled={saving || loading}><Save size={14}/>{saving ? "Saving…" : "Save layout"}</button>}{writable && <button className="primary" onClick={() => void scanAll()} disabled={scanning || loading || layout.aps.length === 0}><ScanLine size={14}/>{scanning ? "Scanning radios…" : "Collect RSSI"}</button>}</div>
    </div>
    {error && <div className="wifi-inline-error">{error}</div>}
    <div className="wifi-layout-controls">
      <label>Width (m)<input type="number" min={2} max={100} step={0.5} value={layout.width_m} onChange={(e) => setLayout((old) => ({ ...old, width_m: Number(e.target.value) || 2 }))}/></label>
      <label>Height (m)<input type="number" min={2} max={100} step={0.5} value={layout.height_m} onChange={(e) => setLayout((old) => ({ ...old, height_m: Number(e.target.value) || 2 }))}/></label>
      <button className="secondary" onClick={addRoom} disabled={!writable}><Plus size={14}/> Add room</button>
      <button className={wallMode ? "primary" : "secondary"} onClick={() => { setWallMode(!wallMode); setWallStart(null); }} disabled={!writable}><Ruler size={14}/>{wallMode ? "Click wall end…" : "Draw wall"}</button>
      <label>Wall material<select value={wallMaterial} onChange={(e) => setWallMaterial(e.target.value)}><option value="brick">Brick / concrete</option><option value="drywall">Drywall</option><option value="glass">Glass</option><option value="open">Open / light partition</option></select></label>
    </div>
    <div className="wifi-canvas">
      <svg viewBox={"-0.25 -0.25 " + (layout.width_m + 0.5) + " " + (layout.height_m + 0.5)} role="img" aria-label="Editable home floor plan" onPointerMove={pointerMove} onPointerUp={() => setDragging(null)} onPointerLeave={() => setDragging(null)} onClick={canvasClick}>
        <defs><pattern id="wifi-grid-pattern" width="0.5" height="0.5" patternUnits="userSpaceOnUse"><path d="M .5 0 L 0 0 0 .5" fill="none" className="wifi-grid"/></pattern></defs>
        <rect x="0" y="0" width={layout.width_m} height={layout.height_m} fill="url(#wifi-grid-pattern)" stroke="#647087" strokeWidth=".06"/>
        {layout.rooms.map((room) => <g key={room.id}><rect className="wifi-room" x={room.x} y={room.y} width={room.width} height={room.height}/><text className="wifi-room-label" x={room.x + 0.12} y={room.y + 0.35}>{room.name}</text></g>)}
        {layout.walls.map((wall) => <line key={wall.id} className="wifi-wall" x1={wall.x1} y1={wall.y1} x2={wall.x2} y2={wall.y2}/>)}
        {wallStart && <circle cx={wallStart.x} cy={wallStart.y} r=".12" fill="#e9bd7a"/>}
        {layout.aps.map((ap) => { const device = deviceMap.get(ap.device_id); return <g key={ap.device_id} className="wifi-ap" transform={"translate(" + ap.x + " " + ap.y + ")"} onPointerDown={(e) => { e.stopPropagation(); setDragging(ap.device_id); e.currentTarget.setPointerCapture(e.pointerId); }} onClick={(e) => e.stopPropagation()}><circle r=".22"/><circle r=".34" fill="none" stroke="#a99aff" strokeWidth=".035" opacity=".7"/><text x=".32" y=".07">{device?.name ?? ap.label}</text></g>; })}
      </svg>
    </div>
    <div className="wifi-layout-bottom">
      <div className="wifi-panel"><h3>Access point placement</h3><div className="wifi-ap-list">{devices.length === 0 ? <p className="wifi-note">Add devices to inventory before placing them on the plan.</p> : devices.map((device) => {
        const placement = layout.aps.find((ap) => ap.device_id === device.id);
        return <div className="wifi-ap-row" key={device.id}><input aria-label={"Place " + device.name} type="checkbox" checked={Boolean(placement)} disabled={!writable} onChange={(e) => toggleAP(device, e.target.checked)}/><div><b>{device.name}</b><small>{device.address} · {placement ? placement.x.toFixed(1) + " m, " + placement.y.toFixed(1) + " m" : "Not placed"}</small></div>{placement && <button onClick={() => setLayout((old) => ({ ...old, aps: old.aps.map((ap) => ap.device_id === device.id ? { ...ap, x: Math.max(0, ap.x - 0.5) } : ap) }))} title="Nudge left">←</button>}</div>;
      })}</div>
        {layout.rooms.length > 0 && <div className="wifi-room-edit"><h3>Rooms</h3>{layout.rooms.map((room) => <div className="wifi-edit-row" key={room.id}><input aria-label={"Room name " + room.name} value={room.name} disabled={!writable} onChange={(e) => setLayout((old) => ({ ...old, rooms: old.rooms.map((r) => r.id === room.id ? { ...r, name: e.target.value } : r) }))}/><button disabled={!writable} title={"Remove " + room.name} onClick={() => setLayout((old) => ({ ...old, rooms: old.rooms.filter((r) => r.id !== room.id) }))}><Trash2 size={13}/></button></div>)}</div>}
        {layout.walls.length > 0 && <div className="wifi-room-edit"><h3>Walls ({layout.walls.length})</h3>{layout.walls.map((wall, i) => <div className="wifi-edit-row" key={wall.id}><span>Wall {i + 1} · {wall.material}</span><button disabled={!writable} title="Remove wall" onClick={() => setLayout((old) => ({ ...old, walls: old.walls.filter((w) => w.id !== wall.id) }))}><Trash2 size={13}/></button></div>)}</div>}
        <p className="wifi-note">Drag AP markers to approximate positions. Add rooms, rename them, and draw walls by clicking two points on the plan. Layout changes are shared only after saving.</p>
      </div>
      <div className="wifi-panel"><h3><RadioTower size={14} style={{ verticalAlign: "middle", marginRight: 5 }}/>Neighbour radio observations <span style={{ color: "#858b9b", fontWeight: 400 }}>({observations.length})</span></h3>
        <div className="wifi-observations"><table><thead><tr><th>RECEIVING AP</th><th>RADIO</th><th>OBSERVED BSSID / SSID</th><th>CHANNEL</th><th>RSSI</th><th>SEEN</th></tr></thead><tbody>{recent.length === 0 ? <tr><td colSpan={6} className="empty">No observations yet. Place enrolled APs and select Collect RSSI.</td></tr> : recent.map((o) => <tr key={o.id}><td><b>{o.source_name}</b></td><td>{o.source_radio}</td><td><b>{bssidOwners.get(o.observed_bssid.toUpperCase()) || "Unmapped neighbour"}</b><code>{o.observed_bssid}</code><small>{o.ssid || "Hidden / unknown SSID"}</small></td><td>{o.channel || "—"}</td><td className={"wifi-signal " + (o.signal_dbm >= -60 ? "strong" : o.signal_dbm >= -70 ? "medium" : "weak")}>{o.signal_dbm} dBm</td><td className="last-seen">{new Date(o.observed_at).toLocaleString()}</td></tr>)}</tbody></table></div>
        <p className="wifi-note">RSSI values here are measured by the AP radio, not predicted. Only the latest 120 records are shown; the database retains up to 30 days. Scans are manual and sequential because radio scans can briefly affect wireless service.</p>
      </div>
    </div>
  </section>;
}
