import { useEffect, useMemo, useRef, useState } from "react";
import { Plus, Wifi, Move, RotateCcw, Info, Trash2, Signal, Ruler } from "lucide-react";

type Device = { id: string; name: string; address: string; model: string; firmware: string; status: string };
type Placement = { deviceId: string; x: number; y: number; radius: number; band: "2.4 GHz" | "5 GHz" | "Dual band" };
type Layout = { width: number; height: number; placements: Placement[] };
const STORAGE_KEY = "openmanage.wifi-coverage.v1";
const DEFAULT_LAYOUT: Layout = { width: 20, height: 12, placements: [] };
const COLORS = ["#7c8cff", "#3bd4b4", "#f4b860", "#ed7eac", "#69b8f7", "#b59cff"];

function loadLayout(): Layout {
  try {
    const raw = localStorage.getItem(STORAGE_KEY);
    if (!raw) return DEFAULT_LAYOUT;
    const parsed = JSON.parse(raw) as Partial<Layout>;
    if (!Array.isArray(parsed.placements)) return DEFAULT_LAYOUT;
    const width = Number(parsed.width), height = Number(parsed.height);
    const placements = parsed.placements.filter((p): p is Placement =>
      Boolean(p) && typeof p.deviceId === "string" && Number.isFinite(p.x) && Number.isFinite(p.y) &&
      Number.isFinite(p.radius) && (p.band === "2.4 GHz" || p.band === "5 GHz" || p.band === "Dual band")
    ).map(p => ({ ...p, x: Math.max(0.04, Math.min(0.96, p.x)), y: Math.max(0.06, Math.min(0.94, p.y)), radius: Math.max(2, Math.min(15, p.radius)) }));
    return { width: Number.isFinite(width) ? Math.max(5, Math.min(60, width)) : 20, height: Number.isFinite(height) ? Math.max(5, Math.min(60, height)) : 12, placements };
  } catch { return DEFAULT_LAYOUT; }
}

export function WifiCoverage({ devices }: { devices: Device[] }) {
  const [layout, setLayout] = useState<Layout>(loadLayout);
  const [selectedId, setSelectedId] = useState("");
  const [activeId, setActiveId] = useState<string | null>(null);
  const [showFootprints, setShowFootprints] = useState(true);
  const [showGrid, setShowGrid] = useState(true);
  const [notice, setNotice] = useState("");
  const svgRef = useRef<SVGSVGElement | null>(null);
  const dragId = useRef<string | null>(null);
  const candidateDevices = devices.filter(d => !layout.placements.some(p => p.deviceId === d.id));
  const resolved = useMemo(() => layout.placements.map((p, i) => ({ ...p, color: COLORS[i % COLORS.length], device: devices.find(d => d.id === p.deviceId) })).filter(p => p.device), [layout.placements, devices]);
  const active = resolved.find(p => p.deviceId === activeId) ?? resolved[0];
  const scaleX = 900 / layout.width, scaleY = 500 / layout.height;

  useEffect(() => {
    try { localStorage.setItem(STORAGE_KEY, JSON.stringify(layout)); setNotice(""); }
    catch { setNotice("Browser storage is unavailable; layout changes will not persist after reload."); }
  }, [layout]);
  useEffect(() => {
    if (activeId && !resolved.some(p => p.deviceId === activeId)) setActiveId(null);
  }, [activeId, resolved]);
  useEffect(() => {
    const endDrag = () => { dragId.current = null; };
    window.addEventListener("pointerup", endDrag);
    window.addEventListener("pointercancel", endDrag);
    return () => { window.removeEventListener("pointerup", endDrag); window.removeEventListener("pointercancel", endDrag); };
  }, []);

  function addPlacement() {
    if (!selectedId || layout.placements.some(p => p.deviceId === selectedId)) return;
    const next: Placement = { deviceId: selectedId, x: 0.5, y: 0.5, radius: 8, band: "Dual band" };
    setLayout(prev => ({ ...prev, placements: [...prev.placements, next] }));
    setActiveId(selectedId); setSelectedId("");
  }
  function updatePlacement(deviceId: string, patch: Partial<Placement>) {
    setLayout(prev => ({ ...prev, placements: prev.placements.map(p => p.deviceId === deviceId ? { ...p, ...patch } : p) }));
  }
  function removePlacement(deviceId: string) {
    setLayout(prev => ({ ...prev, placements: prev.placements.filter(p => p.deviceId !== deviceId) }));
    if (activeId === deviceId) setActiveId(null);
  }
  function resetLayout() {
    if (!window.confirm("Reset the Wi-Fi coverage layout? AP placements and room dimensions will be cleared from this browser.")) return;
    setLayout(DEFAULT_LAYOUT); setActiveId(null);
  }
  function movePoint(event: React.PointerEvent<SVGSVGElement>) {
    if (!dragId.current || !svgRef.current) return;
    const bounds = svgRef.current.getBoundingClientRect();
    if (!bounds.width || !bounds.height) return;
    const x = Math.max(0.04, Math.min(0.96, ((event.clientX - bounds.left) / bounds.width)));
    const y = Math.max(0.06, Math.min(0.94, ((event.clientY - bounds.top) / bounds.height)));
    updatePlacement(dragId.current, { x, y });
  }
  function pointLabel(deviceId: string) { return devices.find(d => d.id === deviceId)?.name ?? "Access point"; }

  return <section className="wifi-coverage-section">
    <div className="section-head wifi-coverage-head">
      <div><div className="eyebrow">RADIO PLANNING / ESTIMATED</div><h2>Wi-Fi coverage map</h2><p>Arrange your access points on a scaled room canvas and compare their estimated footprints.</p></div>
      <div className="wifi-coverage-actions">
        <label className="wifi-select-label" htmlFor="wifi-ap-picker">Add access point</label>
        <select id="wifi-ap-picker" className="secondary" value={selectedId} onChange={e => setSelectedId(e.target.value)} disabled={!candidateDevices.length}>
          <option value="">{candidateDevices.length ? "Select an inventory device…" : "All devices placed"}</option>
          {candidateDevices.map(d => <option key={d.id} value={d.id}>{d.name} · {d.address}</option>)}
        </select>
        <button className="primary" onClick={addPlacement} disabled={!selectedId}><Plus size={15}/> Add AP</button>
      </div>
    </div>
    <div className="wifi-coverage-card">
      <div className="wifi-coverage-toolbar">
        <div className="wifi-coverage-metrics"><span><b>{resolved.length.toString().padStart(2,"0")}</b><small>Plotted APs</small></span><i/><span><b>{layout.width} × {layout.height} m</b><small>Room dimensions</small></span><i/><span><b>Planning estimate</b><small>Not a live RF survey</small></span></div>
        <div className="wifi-coverage-toggles">
          <label><input type="checkbox" checked={showFootprints} onChange={e => setShowFootprints(e.target.checked)}/> Coverage</label>
          <label><input type="checkbox" checked={showGrid} onChange={e => setShowGrid(e.target.checked)}/> Grid</label>
          <button className="row-action" title="Reset coverage layout" aria-label="Reset coverage layout" onClick={resetLayout}><RotateCcw size={15}/></button>
        </div>
      </div>
      <div className="wifi-coverage-workspace">
        <div className="wifi-map-wrap">
          <div className="wifi-map-caption"><span><Move size={13}/> Drag AP markers to reposition</span><span><Ruler size={13}/> Approx. 1 m grid</span></div>
          <svg ref={svgRef} className="wifi-map" viewBox="0 0 900 500" role="img" aria-label="Estimated Wi-Fi coverage plan. Drag access point markers to reposition them." onPointerMove={movePoint} onPointerUp={() => { dragId.current = null; }} onPointerLeave={() => { if (dragId.current) dragId.current = null; }}>
            <defs>
              <linearGradient id="wifi-floor" x1="0" y1="0" x2="1" y2="1"><stop offset="0%" stopColor="#151d2b"/><stop offset="100%" stopColor="#101722"/></linearGradient>
              <pattern id="wifi-grid-small" width={scaleX} height={scaleY} patternUnits="userSpaceOnUse"><path d={`M ${scaleX} 0 L 0 0 0 ${scaleY}`} fill="none" stroke="#52617a" strokeWidth=".55" opacity=".35"/></pattern>
              <pattern id="wifi-grid-large" width={scaleX*5} height={scaleY*5} patternUnits="userSpaceOnUse"><path d={`M ${scaleX*5} 0 L 0 0 0 ${scaleY*5}`} fill="none" stroke="#71809b" strokeWidth="1" opacity=".34"/></pattern>
              {resolved.map(p => <radialGradient key={p.deviceId} id={`wifi-gradient-${p.deviceId.replace(/[^a-zA-Z0-9_-]/g,"")}`}><stop offset="0%" stopColor={p.color} stopOpacity=".34"/><stop offset="45%" stopColor={p.color} stopOpacity=".2"/><stop offset="78%" stopColor={p.color} stopOpacity=".08"/><stop offset="100%" stopColor={p.color} stopOpacity="0"/></radialGradient>)}
            </defs>
            <rect width="900" height="500" rx="12" fill="url(#wifi-floor)"/>
            {showGrid && <><rect width="900" height="500" fill="url(#wifi-grid-small)"/><rect width="900" height="500" fill="url(#wifi-grid-large)"/></>}
            <rect x="1" y="1" width="898" height="498" rx="12" fill="none" stroke="#68758d" strokeWidth="2" opacity=".7"/>
            <text x="18" y="24" fill="#9ca9bf" fontSize="10" letterSpacing="1.5">ROOM PLAN · {layout.width} M × {layout.height} M</text>
            {showFootprints && resolved.map(p => <g key={p.deviceId} pointerEvents="none">
              <ellipse cx={p.x*900} cy={p.y*500} rx={p.radius*scaleX} ry={p.radius*scaleY} fill={`url(#wifi-gradient-${p.deviceId.replace(/[^a-zA-Z0-9_-]/g,"")})`}/>
              <ellipse cx={p.x*900} cy={p.y*500} rx={p.radius*scaleX} ry={p.radius*scaleY} fill="none" stroke={p.color} strokeWidth="1.5" strokeDasharray="5 5" opacity=".75"/>
              <ellipse cx={p.x*900} cy={p.y*500} rx={p.radius*scaleX*.68} ry={p.radius*scaleY*.68} fill="none" stroke={p.color} strokeWidth="1" opacity=".55"/>
            </g>)}
            {resolved.map(p => <g key={p.deviceId} className={"wifi-ap-marker" + (active?.deviceId === p.deviceId ? " selected" : "")} transform={`translate(${p.x*900} ${p.y*500})`} onPointerDown={e => { e.stopPropagation(); dragId.current = p.deviceId; setActiveId(p.deviceId); svgRef.current?.setPointerCapture(e.pointerId); }} onClick={() => setActiveId(p.deviceId)} role="button" aria-label={`Select ${pointLabel(p.deviceId)} access point`} tabIndex={0} onKeyDown={e => { if (e.key === "Enter" || e.key === " ") setActiveId(p.deviceId); }}>
              <circle r="24" fill={p.color} opacity=".12"/><circle r="17" fill="#111925" stroke={p.color} strokeWidth="2.5"/><circle r="4" fill={p.color}/>
              <path d="M -8 -5 Q 0 -13 8 -5 M -5 -1 Q 0 -6 5 -1" fill="none" stroke="#f4f7ff" strokeWidth="1.5" strokeLinecap="round"/>
              <rect x="-58" y="22" width="116" height="25" rx="6" fill="#0c121d" stroke={p.color} strokeOpacity=".65"/>
              <text x="0" y="38" fill="#f1f4fa" textAnchor="middle" fontSize="10" fontWeight="600">{pointLabel(p.deviceId).slice(0,18)}</text>
            </g>)}
            {resolved.length === 0 && <g><rect x="250" y="185" width="400" height="112" rx="14" fill="#151e2c" stroke="#39465a"/><text x="450" y="224" textAnchor="middle" fill="#e4eaf5" fontSize="17" fontWeight="600">Start your coverage plan</text><text x="450" y="249" textAnchor="middle" fill="#9da9bd" fontSize="12">Add an access point from your device inventory above.</text><text x="450" y="270" textAnchor="middle" fill="#75839b" fontSize="10">Drag markers to model their approximate location.</text></g>}
            <g transform="translate(865 475)" opacity=".75"><path d="M 0 0 L 0 -22 M -5 -16 L 0 -22 L 5 -16" stroke="#a8b5cb" strokeWidth="1.5" fill="none"/><text x="0" y="15" fill="#a8b5cb" textAnchor="middle" fontSize="9">N</text></g>
          </svg>
          <div className="wifi-map-scale"><span>0 m</span><span>{Math.round(layout.width/2)} m</span><span>{layout.width} m</span></div>
        </div>
        <aside className="wifi-coverage-side">
          <div className="wifi-side-title"><div><b>AP placement</b><small>{resolved.length} configured</small></div><Signal size={17}/></div>
          {resolved.length === 0 ? <div className="wifi-empty-side"><Wifi size={22}/><p>No access points plotted yet.</p></div> : <div className="wifi-ap-list">{resolved.map(p => <button key={p.deviceId} className={"wifi-ap-item"+(active?.deviceId===p.deviceId?" active":"")} onClick={() => setActiveId(p.deviceId)}><span className="wifi-ap-swatch" style={{background:p.color}}/><span className="wifi-ap-copy"><b>{p.device?.name}</b><small>{p.device?.address} · {p.device?.status}</small></span><span className="wifi-ap-radius">{p.radius} m</span></button>)}</div>}
          {active && <div className="wifi-ap-editor">
            <div className="wifi-editor-heading"><span className="wifi-ap-swatch" style={{background:active.color}}/><div><b>{active.device?.name}</b><small>Estimated signal footprint</small></div></div>
            <label className="wifi-field-label">Estimated coverage radius <b>{active.radius} m</b></label>
            <input className="wifi-range" type="range" min="2" max="15" step="1" value={active.radius} onChange={e => updatePlacement(active.deviceId,{radius:Number(e.target.value)})}/>
            <div className="wifi-range-labels"><span>2 m · compact</span><span>15 m · broad</span></div>
            <label className="wifi-field-label" htmlFor="wifi-band">Radio profile</label>
            <select id="wifi-band" className="wifi-band-select" value={active.band} onChange={e => updatePlacement(active.deviceId,{band:e.target.value as Placement["band"]})}><option>Dual band</option><option>2.4 GHz</option><option>5 GHz</option></select>
            <div className="wifi-coordinates"><span>Position X <b>{Math.round(active.x*layout.width*10)/10} m</b></span><span>Position Y <b>{Math.round(active.y*layout.height*10)/10} m</b></span></div>
            <button className="wifi-remove-ap" onClick={() => removePlacement(active.deviceId)}><Trash2 size={14}/> Remove from map</button>
          </div>}
          <div className="wifi-coverage-legend"><b>Coverage intensity</b><div className="wifi-gradient-key"/><div className="wifi-legend-labels"><span>Higher near AP</span><span>Lower at edge</span></div><p><Info size={13}/> Overlapping colors indicate estimated overlap, not measured signal strength.</p></div>
        </aside>
      </div>
      <div className="wifi-coverage-footer">
        <div className="wifi-dimensions"><Ruler size={15}/><span>Room width</span><input aria-label="Room width in metres" type="number" min="5" max="60" value={layout.width} onChange={e => setLayout(prev=>({...prev,width:Math.max(5,Math.min(60,Number(e.target.value)||5))}))}/><span>m</span><span className="wifi-dimension-separator">×</span><span>height</span><input aria-label="Room height in metres" type="number" min="5" max="60" value={layout.height} onChange={e => setLayout(prev=>({...prev,height:Math.max(5,Math.min(60,Number(e.target.value)||5))}))}/><span>m</span></div>
        <span className="wifi-storage-note">Layout is saved in this browser only</span>
      </div>
    </div>
    <div className="wifi-disclaimer"><Info size={15}/><span><b>Planning estimate, not a site survey.</b> Coverage uses a simplified radial model. Walls, floors, interference, antenna orientation, transmit power, client capability, and channel utilization are not measured. Use a Wi-Fi analyzer or survey for actual RSSI/SNR validation.</span></div>
    {notice && <div className="notice">{notice}</div>}
  </section>;
}
