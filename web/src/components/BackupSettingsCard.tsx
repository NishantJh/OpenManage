import { useEffect, useState } from "react";
import { HardDriveDownload } from "lucide-react";
import { apiJSON } from "../api";
import type { MonitoringSettings } from "../types";

const intervals:[number,string][]=[[86400,"Every day"],[259200,"Every 3 days"],[604800,"Every week"],[2592000,"Every 30 days"]];

// Scheduled device backups and how many each device keeps. Both are administrator
// settings; retention applies to every device and takes effect at its next stored backup.
// The save sends the other settings as the server last returned them.
export default function BackupSettingsCard({settings,isAdmin,onSaved}:{settings:MonitoringSettings|null;isAdmin:boolean;onSaved:(s:MonitoringSettings)=>void}){
 const [interval,setInterval]=useState(86400),[retention,setRetention]=useState("5"),[busy,setBusy]=useState(false),[message,setMessage]=useState("");
 useEffect(()=>{if(settings){setInterval(settings.backup_interval_seconds);setRetention(String(settings.backup_retention))}},[settings?.backup_interval_seconds,settings?.backup_retention]);
 const enabled=Boolean(settings?.backups_enabled);
 const keep=Math.min(20,Math.max(1,Math.round(Number(retention))||1));
 async function save(nextEnabled:boolean){if(!settings)return;setBusy(true);setMessage("");try{const data=await apiJSON<MonitoringSettings>("/api/monitoring/settings","PUT",{enabled:settings.enabled,interval_seconds:settings.interval_seconds,backups_enabled:nextEnabled,backup_interval_seconds:interval,backup_retention:keep},"Could not save backup settings");onSaved(data);setMessage("Backup settings saved.")}catch(e){setMessage(e instanceof Error?e.message:"Could not save backup settings")}finally{setBusy(false)}}
 const label=intervals.find(([s])=>s===settings?.backup_interval_seconds)?.[1].toLowerCase()??"on a custom interval";
 const changed=Boolean(settings)&&(interval!==settings?.backup_interval_seconds||keep!==settings?.backup_retention);
 return <div className="info-card blue"><HardDriveDownload size={20}/><div><b>Device backups</b><p>{enabled?"Each enrolled device is backed up "+label+". An unchanged device does not add a copy.":"Scheduled backups are off. You can still back up any device by hand below."} Each device keeps its newest <b>{settings?.backup_retention??5}</b> backups; a backup pinned for a firmware upgrade is kept on top of that. A lower limit trims older backups when the device next stores a new one.</p>
 {isAdmin&&settings&&<div className="snapshot-controls"><select aria-label="Backup interval" className="secondary" value={interval} onChange={e=>setInterval(Number(e.target.value))} disabled={busy}>{intervals.map(([s,l])=><option key={s} value={s}>{l}</option>)}</select><label className="inline-field">Keep per device<input aria-label="Backups kept per device" type="number" min={1} max={20} value={retention} onChange={e=>setRetention(e.target.value)} onBlur={()=>setRetention(String(keep))} disabled={busy}/></label>{enabled&&<button className="secondary" onClick={()=>void save(true)} disabled={busy||!changed}>Save settings</button>}{!enabled&&changed&&<button className="secondary" onClick={()=>void save(false)} disabled={busy}>Save retention</button>}<button className={enabled?"primary":"secondary"} onClick={()=>void save(!enabled)} disabled={busy}>{busy?"Saving…":enabled?"Disable scheduled backups":"Enable scheduled backups"}</button></div>}
 {message&&<p>{message}</p>}<span>{enabled?"BACKUPS ON":"OPT-IN"}</span></div></div>;
}
