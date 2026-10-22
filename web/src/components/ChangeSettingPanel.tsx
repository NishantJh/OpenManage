import { useEffect, useState } from "react";
import type { FormEvent } from "react";
import { Eye, ShieldCheck } from "lucide-react";
import { apiFetch, apiJSON } from "../api";
import type { ConfigChangeRecord, Device, WritableSetting } from "../types";

const statusTone:Record<ConfigChangeRecord["status"],string>={previewed:"unknown",applying:"degraded",applied:"online",rolled_back:"degraded",failed:"offline",expired:"unknown"};
const statusText:Record<ConfigChangeRecord["status"],string>={previewed:"PREVIEWED",applying:"APPLYING",applied:"APPLIED",rolled_back:"ROLLED BACK",failed:"FAILED",expired:"EXPIRED"};

// Controlled changes: preview reads the device and changes nothing; apply arms rpcd's
// timed rollback, verifies over a new connection, and only then confirms.
export default function ChangeSettingPanel({device,onChanged}:{device:Device;onChanged:()=>void}){
 const [settings,setSettings]=useState<WritableSetting[]>([]),[setting,setSetting]=useState(""),[value,setValue]=useState("");
 const [preview,setPreview]=useState<ConfigChangeRecord|null>(null),[history,setHistory]=useState<ConfigChangeRecord[]>([]),[busy,setBusy]=useState(false),[message,setMessage]=useState<{text:string;tone:"notice"|"error"}|null>(null);
 async function loadHistory(){try{const r=await apiFetch("/api/devices/"+device.id+"/config-changes");if(r.ok)setHistory(await r.json())}catch{/* history is optional */}}
 useEffect(()=>{void (async()=>{try{const data=await apiJSON<WritableSetting[]>("/api/config-settings","GET",undefined,"Could not load settings");setSettings(data);if(data[0])setSetting(data[0].key)}catch(e){setMessage({tone:"error",text:e instanceof Error?e.message:"Could not load settings"})}})();void loadHistory()},[device.id]);
 async function doPreview(e:FormEvent){e.preventDefault();setBusy(true);setMessage(null);setPreview(null);try{setPreview(await apiJSON<ConfigChangeRecord>("/api/devices/"+device.id+"/config-changes","POST",{setting,value},"Could not preview the change"));void loadHistory()}catch(e){setMessage({tone:"error",text:e instanceof Error?e.message:"Could not preview the change"})}finally{setBusy(false)}}
 async function apply(){if(!preview)return;setBusy(true);setMessage(null);try{const data=await apiJSON<ConfigChangeRecord>("/api/devices/"+device.id+"/config-changes/"+preview.id+"/apply","POST",undefined,"Could not apply the change");setPreview(null);setValue("");setMessage({tone:data.status==="applied"?"notice":"error",text:data.status==="applied"?"Applied and confirmed after checking the router over a new connection.":(data.status==="rolled_back"?"Rolled back: ":"Not applied: ")+data.detail+"."});onChanged()}catch(e){setMessage({tone:"error",text:e instanceof Error?e.message:"Could not apply the change"})}finally{setBusy(false);void loadHistory()}}
 const help=settings.find(s=>s.key===setting)?.help;
 return <div className="change-panel"><h3>Change a setting</h3><p className="field-help">Preview reads the router and changes nothing. Applying arms OpenWrt's automatic rollback, checks the new value over a fresh connection, and only then confirms; if that check fails the old value is restored.</p>
 <form className="change-form" onSubmit={doPreview}><label>Setting<select className="secondary" aria-label="Setting to change" value={setting} onChange={e=>{setSetting(e.target.value);setPreview(null)}}>{settings.map(s=><option key={s.key} value={s.key}>{s.label}</option>)}</select></label><label>New value<input aria-label="New value" required value={value} onChange={e=>{setValue(e.target.value);setPreview(null)}} placeholder="e.g. hall-ap"/>{help&&<small className="field-help">{help}</small>}</label><button className="secondary" disabled={busy||!setting}><Eye size={15}/> Preview</button></form>
 {message&&<div className={message.tone}>{message.text}</div>}
 {preview&&<div className="change-preview"><div><b>{preview.label}</b><div className="change-diff"><span className="config-value">{preview.old_value||"(not set)"}</span> → <span className="config-value">{preview.new_value}</span></div><small className="field-help">Valid until {new Date(preview.expires_at).toLocaleTimeString()}. Nothing has changed on the router yet.</small></div><button className="primary" onClick={()=>void apply()} disabled={busy}>{busy?"Applying…":<><ShieldCheck size={15}/> Apply with rollback</>}</button></div>}
 {history.length>0&&<div className="table-card"><div className="table-scroll"><table><thead><tr><th>SETTING</th><th>CHANGE</th><th>RESULT</th><th>WHEN</th></tr></thead><tbody>{history.slice(0,10).map(c=><tr key={c.id}><td>{c.label||c.setting}</td><td><span className="config-value">{c.old_value||"(not set)"}</span> → <span className="config-value">{c.new_value}</span></td><td><span className={"status "+statusTone[c.status]} title={c.detail||undefined}><i/>{statusText[c.status]}</span>{c.detail&&<small>{c.detail}</small>}</td><td className="last-seen">{new Date(c.finished_at??c.created_at).toLocaleString()}</td></tr>)}</tbody></table></div></div>}
 </div>;
}
