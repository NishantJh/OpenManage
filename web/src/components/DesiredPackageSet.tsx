import { useEffect, useMemo, useRef, useState } from "react";
import { apiJSON } from "../api";
import type { DevicePackageSet, DevicePackageSetRevision, PackageSource, Role } from "../types";

type DraftSource = { name: string; mode: "pinned" | "latest"; source_id?: number };
const splitNames = (value: string) => value.split(/[\s,]+/).map(x => x.trim()).filter(Boolean);
const sourceLabel = (s: PackageSource) => s.name + " " + s.version + " · " + s.arch + " · " + s.format;

export default function DesiredPackageSet({ deviceId, deviceName, role, onChanged }: { deviceId: string; deviceName: string; role: Role; onChanged: () => void }) {
 const admin = role === "admin", canReadSources = admin || role === "operator";
 const [data, setData] = useState<DevicePackageSet | null>(null);
 const [sources, setSources] = useState<PackageSource[]>([]);
 const [official, setOfficial] = useState(""), [removed, setRemoved] = useState("");
 const [draftSources, setDraftSources] = useState<DraftSource[]>([]);
 const [selectedSource, setSelectedSource] = useState("");
 const [trackingMode, setTrackingMode] = useState<"pinned" | "latest">("pinned");
 const [loading, setLoading] = useState(true), [saving, setSaving] = useState(false), [restoring, setRestoring] = useState<number | null>(null);
 const [error, setError] = useState(""), [notice, setNotice] = useState("");
 const current = data?.current;
 const requestRef = useRef(0);
 const confirmedSources = useMemo(() => sources.filter(s => s.status === "confirmed"), [sources]);

 async function load() {
  if (!deviceId) return;
  const request = ++requestRef.current;
  setLoading(true); setError("");
  try {
   const [set, available] = await Promise.all([
    apiJSON<DevicePackageSet>("/api/devices/" + deviceId + "/package-set", "GET", undefined, "Could not load the desired package set"),
    canReadSources ? apiJSON<PackageSource[]>("/api/package-sources", "GET", undefined, "Could not load confirmed package sources") : Promise.resolve([] as PackageSource[])
   ]);
   if (request === requestRef.current) { setData(set); setSources(available); }
  } catch (e) { if (request === requestRef.current) setError(e instanceof Error ? e.message : "Could not load the desired package set"); }
  finally { if (request === requestRef.current) setLoading(false); }
 }
 useEffect(() => { setData(null); setSources([]); setNotice(""); setError(""); void load(); }, [deviceId, canReadSources]);
 useEffect(() => {
  setOfficial((current?.official_packages ?? []).join("\n"));
  setRemoved((current?.removed_defaults ?? []).join("\n"));
  setDraftSources((current?.sources ?? []).map(s => ({ name: s.name, mode: s.mode, ...(s.source_id ? { source_id: s.source_id } : {}) })));
 }, [data?.current_revision]);

 const addSource = () => {
  const selected = confirmedSources.find(s => String(s.id) === selectedSource);
  if (!selected) return;
  if (draftSources.some(s => s.name === selected.name)) { setError("That package already has a source entry."); return; }
  setDraftSources(prev => [...prev, { name: selected.name, mode: trackingMode, ...(trackingMode === "pinned" ? { source_id: selected.id } : {}) }].sort((a,b) => a.name.localeCompare(b.name)));
  setSelectedSource(""); setError("");
 };
 const save = async () => {
  setSaving(true); setError(""); setNotice("");
  try {
   const updated = await apiJSON<DevicePackageSet>("/api/devices/" + deviceId + "/package-set", "PUT", {
    official_packages: splitNames(official), removed_defaults: splitNames(removed),
    sources: draftSources.map(s => ({ name: s.name, mode: s.mode, ...(s.mode === "pinned" ? { source_id: s.source_id } : {}) }))
   }, "Could not save the desired package set");
   setData(updated); setNotice("Saved revision " + updated.current_revision + ". No packages were changed on " + deviceName + ".");
   setOfficial((updated.current?.official_packages ?? []).join("\n")); setRemoved((updated.current?.removed_defaults ?? []).join("\n"));
   setDraftSources((updated.current?.sources ?? []).map(s => ({ name:s.name, mode:s.mode, ...(s.source_id ? {source_id:s.source_id} : {}) })));
   onChanged();
  } catch (e) { setError(e instanceof Error ? e.message : "Could not save the desired package set"); }
  finally { setSaving(false); }
 };
 const restore = async (revision: DevicePackageSetRevision) => {
  if (!window.confirm("Restore revision " + revision.revision + " for " + deviceName + "? This creates a new revision; history is not rewritten. It does not change the device.")) return;
  setRestoring(revision.revision); setError(""); setNotice("");
  try {
   const updated = await apiJSON<DevicePackageSet>("/api/devices/" + deviceId + "/package-set/revisions/" + revision.revision + "/restore", "POST", {}, "Could not restore this revision");
   setData(updated); setNotice("Restored revision " + revision.revision + " as new revision " + updated.current_revision + ". No packages were changed on the device."); onChanged();
  } catch (e) { setError(e instanceof Error ? e.message : "Could not restore this revision"); }
  finally { setRestoring(null); }
 };
 const dirty = official !== (current?.official_packages ?? []).join("\n") || removed !== (current?.removed_defaults ?? []).join("\n") || JSON.stringify(draftSources) !== JSON.stringify((current?.sources ?? []).map(s => ({name:s.name,mode:s.mode,...(s.source_id ? {source_id:s.source_id} : {})})));
 return <section className="desired-package-set">
  <div className="section-head"><div><h2>Desired package set</h2><p>Versioned package intent for {deviceName}. Changes affect future firmware plans, not the running device.</p></div><span className="firmware-read">{data?.current_revision ? "Current revision " + data.current_revision : "No revisions yet"}</span></div>
  {error && <div className="error backup-message">{error}</div>}{notice && <div className="notice backup-message">{notice}</div>}
  {loading ? <div className="table-card"><p className="empty-line">Loading desired package set…</p></div> : <>
   <div className="table-card"><div className="table-meta"><span>● &nbsp; Package intent</span><span>{current ? "Revision " + current.revision : "Initial revision"}</span></div>
    <div className="upgrade-body">
     <p>Enter package names separated by commas, spaces, or new lines. Names are validated by the API; a package cannot be both added and removed.</p>
     <div className="desired-set-fields">
      <label>Official packages to add<textarea aria-label="Official packages to add" value={official} onChange={e=>setOfficial(e.target.value)} disabled={!admin} placeholder={"luci-app-example\\n"} rows={4}/><small>Included only when available in the target release's official feeds.</small></label>
      <label>Firmware defaults to remove<textarea aria-label="Firmware defaults to remove" value={removed} onChange={e=>setRemoved(e.target.value)} disabled={!admin} placeholder={"ppp\\n"} rows={4}/><small>Only stock defaults in the target release can be omitted from the image.</small></label>
     </div>
     <div className="desired-sources-head"><b>Custom package sources</b><span>Confirmed registry entries only</span></div>
     {draftSources.length ? <div className="table-scroll"><table><thead><tr><th>PACKAGE</th><th>TRACKING</th><th>VERSION / SOURCE</th>{admin&&<th></th>}</tr></thead><tbody>{draftSources.map(s=>{const src=confirmedSources.find(x=>x.id===s.source_id) ?? confirmedSources.find(x=>x.name===s.name);return <tr key={s.name}><td><b>{s.name}</b></td><td>{s.mode==="pinned"?"Pinned":"Latest confirmed"}</td><td>{s.mode==="pinned"?(src?sourceLabel(src):"Source #"+s.source_id):(src?"Latest at plan time · "+src.version:"Latest confirmed source at plan time")}</td>{admin&&<td><button className="row-action" aria-label={"Remove "+s.name+" from desired package set"} onClick={()=>setDraftSources(prev=>prev.filter(x=>x.name!==s.name))}>Remove</button></td>}</tr>})}</tbody></table></div> : <p className="empty-line">No custom sources in this desired set.</p>}
     {admin&&<div className="snapshot-controls desired-source-controls">
      <select aria-label="Confirmed package source" value={selectedSource} onChange={e=>setSelectedSource(e.target.value)}><option value="">Choose a confirmed source…</option>{confirmedSources.filter(s=>!draftSources.some(d=>d.name===s.name)).map(s=><option key={s.id} value={s.id}>{sourceLabel(s)}</option>)}</select>
      <select aria-label="Source tracking mode" value={trackingMode} onChange={e=>setTrackingMode(e.target.value as "pinned"|"latest")}><option value="pinned">Pin this exact version</option><option value="latest">Follow latest confirmed version</option></select>
      <button className="secondary" disabled={!selectedSource} onClick={addSource}>Add source</button>
      {confirmedSources.length===0&&<small>Register and confirm a package in Software sources below before adding it here.</small>}
     </div>}
     {admin&&<div className="modal-actions desired-actions"><span>{dirty?"Unsaved changes":"No unsaved changes"}</span><button className="primary" disabled={saving||!dirty} onClick={()=>void save()}>{saving?"Saving…":"Save new revision"}</button></div>}
     {!admin&&<p className="firmware-read">Only administrators can edit or restore a desired package set.</p>}
    </div>
   </div>
   <div className="table-card desired-history"><div className="table-meta"><span>● &nbsp; Revision history &nbsp; <b>{data?.revisions.length ?? 0}</b></span><span>Latest 50 revisions</span></div>
    {!data?.revisions.length ? <p className="empty-line">No revisions recorded yet.</p> : <div className="table-scroll"><table><thead><tr><th>REVISION</th><th>CREATED</th><th>BASED ON</th><th>CONTENTS</th>{admin&&<th></th>}</tr></thead><tbody>{data.revisions.map(r=><tr key={r.revision}><td><b>#{r.revision}</b>{r.revision===data.current_revision&&<span className="drift-tag">CURRENT</span>}</td><td>{new Date(r.created_at).toLocaleString()}<small>{r.created_by ? "Actor ID: "+r.created_by : "System actor"}</small></td><td>{r.based_on_revision ? "#"+r.based_on_revision : "—"}</td><td><small>{r.official_packages.length} official additions · {r.removed_defaults.length} default removals · {r.sources.length} custom sources</small></td>{admin&&<td><button className="secondary" disabled={restoring!==null||r.revision===data.current_revision} onClick={()=>void restore(r)}>{restoring===r.revision?"Restoring…":"Restore"}</button></td>}</tr>)}</tbody></table></div>}
   </div>
  </>}
 </section>;
}
