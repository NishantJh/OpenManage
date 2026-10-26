import { AlertTriangle, CheckCircle2, Activity, History } from "lucide-react";
import { navigate } from "../nav";
import type { AuditEvent, Device, MonitoringCheck } from "../types";

function ago(iso:string){const s=Math.max(0,(Date.now()-new Date(iso).getTime())/1000);return s<60?"just now":s<3600?Math.floor(s/60)+" min ago":s<86400?Math.floor(s/3600)+" h ago":Math.floor(s/86400)+" d ago"}
function go(path:string){return (e:React.MouseEvent)=>{e.preventDefault();navigate(path)}}

export default function Attention({devices,monitoring,history,audit,loading}:{devices:Device[];monitoring:boolean;history:MonitoringCheck[];audit:AuditEvent[];loading:boolean}){
 const issues=devices.flatMap(d=>{const out:{id:string;name:string;text:string;tone:string}[]=[];
  if(d.status==="offline")out.push({id:d.id+"o",name:d.name,text:"is offline",tone:"bad"});
  else if(d.status==="degraded")out.push({id:d.id+"g",name:d.name,text:"is degraded",tone:"warn"});
  if(d.config_drift)out.push({id:d.id+"d",name:d.name,text:"has unreviewed configuration changes",tone:"warn"});
  return out});
 const last=history[0];
 return <div className="attention-grid">
  <div className="info-card attention"><AlertTriangle size={20}/><div><b>Needs attention</b>{loading?<p>Loading…</p>:issues.length===0?<p className="ok"><CheckCircle2 size={13}/> All clear. No offline devices or unreviewed changes.</p>:<ul>{issues.slice(0,6).map(i=><li key={i.id} className={i.tone}><b>{i.name}</b> {i.text}</li>)}</ul>}{issues.length>6&&<p>+{issues.length-6} more</p>}<a href="/devices" onClick={go("/devices")}>VIEW DEVICES</a></div></div>
  <div className="info-card blue"><Activity size={20}/><div><b>Monitoring</b><p>{monitoring?"Server-side checks are on.":"Monitoring is off."} {last?<>Last check: <b>{last.device_name}</b> was {last.status} {ago(last.finished_at)}.</>:"No checks recorded yet."}</p><a href="/monitoring" onClick={go("/monitoring")}>{monitoring?"VIEW HISTORY":"SET UP MONITORING"}</a></div></div>
  <div className="info-card"><History size={20}/><div><b>Recent activity</b>{audit.length===0?<p>No audit events to show.</p>:<ul>{audit.slice(0,5).map(e=><li key={e.id}><code>{e.action}</code> by {e.username} · {ago(e.created_at)}</li>)}</ul>}<a href="/access" onClick={go("/access")}>FULL AUDIT LOG</a></div></div>
 </div>;
}
