import { useEffect, useState } from "react";
import { FileClock } from "lucide-react";
import { apiJSON } from "../api";
import type { MonitoringSettings } from "../types";

const intervals:[number,string][]=[[900,"Every 15 minutes"],[3600,"Every hour"],[21600,"Every 6 hours"],[86400,"Every 24 hours"]];

// Scheduled configuration snapshots are opt-in and independent of health monitoring.
// The save sends the monitoring values as the server last returned them, so an
// unsaved monitoring interval in the toolbar is not saved by accident.
export default function SnapshotScheduleCard({settings,isAdmin,onSaved}:{settings:MonitoringSettings|null;isAdmin:boolean;onSaved:(s:MonitoringSettings)=>void}){
 const [snapInterval,setSnapInterval]=useState(3600),[busy,setBusy]=useState(false),[message,setMessage]=useState("");
 useEffect(()=>{if(settings)setSnapInterval(settings.snapshot_interval_seconds)},[settings?.snapshot_interval_seconds]);
 const enabled=Boolean(settings?.snapshots_enabled);
 async function save(nextEnabled:boolean){if(!settings)return;setBusy(true);setMessage("");try{const data=await apiJSON<MonitoringSettings>("/api/monitoring/settings","PUT",{enabled:settings.enabled,interval_seconds:settings.interval_seconds,snapshots_enabled:nextEnabled,snapshot_interval_seconds:snapInterval},"Could not save snapshot schedule");onSaved(data);setMessage(data.snapshots_enabled?"Scheduled snapshots saved.":"Scheduled snapshots disabled.")}catch(e){setMessage(e instanceof Error?e.message:"Could not save snapshot schedule")}finally{setBusy(false)}}
 const label=intervals.find(([s])=>s===settings?.snapshot_interval_seconds)?.[1].toLowerCase()??"on a custom interval";
 return <div className="info-card blue"><FileClock size={20}/><div><b>Scheduled snapshots</b><p>{enabled?"Each enrolled device's configuration is exported "+label+" and compared with the last snapshot. Changes count as drift until an administrator reviews them.":"Scheduled snapshots are off. Turn them on to notice configuration drift without capturing by hand."}</p>
 {isAdmin&&settings&&<div className="snapshot-controls"><select aria-label="Snapshot interval" className="secondary" value={snapInterval} onChange={e=>setSnapInterval(Number(e.target.value))} disabled={busy}>{intervals.map(([s,l])=><option key={s} value={s}>{l}</option>)}</select>{enabled&&<button className="secondary" onClick={()=>void save(true)} disabled={busy||snapInterval===settings.snapshot_interval_seconds}>Save interval</button>}<button className={enabled?"primary":"secondary"} onClick={()=>void save(!enabled)} disabled={busy}>{busy?"Saving…":enabled?"Disable snapshots":"Enable snapshots"}</button></div>}
 {message&&<p>{message}</p>}<span>{enabled?"SNAPSHOTS ON":"OPT-IN · READ-ONLY"}</span></div></div>;
}
