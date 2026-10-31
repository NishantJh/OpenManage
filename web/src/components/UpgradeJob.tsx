import { useEffect, useRef, useState } from "react";
import { Rocket } from "lucide-react";
import { apiJSON } from "../api";
import type { FirmwarePlan, FirmwareUpgrade, Role } from "../types";

const open=(s:string)=>s==="preparing"||s==="ready"||s==="flashing"||s==="verifying";
const mib=(b:number)=>(b/1024/1024).toFixed(1)+" MiB";
const label:Record<string,string>={preparing:"PREPARING",ready:"READY TO FLASH",stopped:"STOPPED",flashing:"FLASHING",verifying:"CHECKING THE DEVICE",done:"DONE",done_with_warnings:"DONE, NEEDS A LOOK",needs_attention:"NEEDS ATTENTION"};
const checkTag:Record<string,string>={ok:"OK",warning:"CHECK",failed:"FAILED"};

// Prepares, flashes and verifies one firmware upgrade. Preparing changes nothing on the
// device beyond one staged file; the flash only starts when an administrator types the
// device's name on a ready job.
export default function UpgradeJob({deviceId,deviceName,version,role,plan,campaignMode=false,campaignActive=true,upgradeId}:{deviceId:string;deviceName:string;version:string;role:Role;plan:FirmwarePlan;campaignMode?:boolean;campaignActive?:boolean;upgradeId?:number}){
 const [jobs,setJobs]=useState<FirmwareUpgrade[]>([]),[busy,setBusy]=useState(false),[error,setError]=useState(""),[typed,setTyped]=useState(""),[seen,setSeen]=useState<string[]>([]);
 const canPrepare=campaignMode?role==="admin":role==="admin"||role==="operator",canFlash=role==="admin"&&(!campaignMode||campaignActive),canAcknowledge=campaignMode?role==="admin":canPrepare,current=useRef(deviceId),job=(upgradeId!==undefined?jobs.find(j=>j.id===upgradeId):jobs[0])??null,active=job!==null&&open(job.status);
 async function load(){const id=deviceId;try{const d=await apiJSON<FirmwareUpgrade[]>("/api/devices/"+id+"/firmware-upgrades","GET",undefined,"Could not load the upgrade");if(current.current===id)setJobs(d)}catch(e){if(current.current===id)setError(e instanceof Error?e.message:"Could not load the upgrade")}}
 useEffect(()=>{current.current=deviceId;setJobs([]);setError("");setTyped("");setSeen([]);void load()},[deviceId,upgradeId]);
 useEffect(()=>{if(!active)return;const t=setInterval(()=>void load(),3000);return()=>clearInterval(t)},[active,deviceId]);
 useEffect(()=>{setTyped("");setSeen([])},[job?.id]);
 async function call(path:string,body?:unknown,fail="Request failed"){const id=deviceId;setBusy(true);setError("");try{await apiJSON("/api/devices/"+id+"/firmware-upgrades"+path,"POST",body,fail);await load()}catch(e){if(current.current===id)setError(e instanceof Error?e.message:fail)}finally{if(current.current===id)setBusy(false)}}
 const jobWarnings:string[]=(()=>{try{return job&&job.plan?(job.plan as FirmwarePlan).warnings??[]:[]}catch{return []}})();
 const allSeen=jobWarnings.every(w=>seen.includes(w)),nameOk=typed===deviceName;
 const unacked=job!==null&&(job.status==="done"||job.status==="done_with_warnings"||job.status==="needs_attention")&&!job.acknowledged_at;
 const checks=job?.result??[];
 return <div className="upgrade-job"><div className="section-head"><div><h2>{campaignMode?"Campaign device upgrade":"Upgrade this device"}</h2><p>Preparing builds the image, takes a backup and stages the image on the device. Nothing is flashed until you confirm.</p></div></div>
 {error&&<div className="error backup-message">{error}</div>}
 {job&&<div className="table-card upgrade-status"><div className="table-meta"><span>● &nbsp; OpenWrt {job.version} &nbsp; <b className={"job-"+job.status}>{label[job.status]??job.status}</b></span>{job.finished_at&&<span>{new Date(job.finished_at).toLocaleString()}</span>}</div>
  <div className="upgrade-body">
   {(job.status==="preparing"||job.status==="flashing"||job.status==="verifying")&&<p><b>{job.step||"Working"}</b>{job.detail&&" · "+job.detail}</p>}
   {(job.status==="flashing"||job.status==="verifying")&&<p className="empty-line">The device restarts and is unreachable for a few minutes. This page updates by itself. Do not unplug the device.</p>}
   {job.status==="ready"&&<>
    <p>Image <code>{job.image_name}</code> · {mib(job.image_size)} · SHA-256 <code>{job.image_sha256.slice(0,16)}…</code></p>
    <p>A backup was taken and is kept until you acknowledge the result.{job.expires_at&&" The staged image expires at "+new Date(job.expires_at).toLocaleTimeString()+"."}</p>
    {jobWarnings.length>0&&<div className="warning plan-warnings">{jobWarnings.map(w=><label key={w} className="check-row"><input type="checkbox" checked={seen.includes(w)} onChange={e=>setSeen(s=>e.target.checked?[...s,w]:s.filter(x=>x!==w))}/> {w}</label>)}</div>}
    {canFlash?<div className="snapshot-controls"><input aria-label="Device name" placeholder={"Type "+deviceName+" to confirm"} value={typed} onChange={e=>setTyped(e.target.value)}/><button className="primary danger" disabled={busy||!nameOk||!allSeen} onClick={()=>void call("/"+job.id+"/flash",{confirm_name:typed,acknowledge_warnings:jobWarnings.length>0&&allSeen},"Could not start the flash")}><Rocket size={15}/> Flash and restart {deviceName}</button></div>:<p className="empty-line">{campaignMode&&!campaignActive?"Campaign is paused; flash confirmation is disabled.":"Only an administrator can flash."}</p>}
    {role==="admin"&&<div className="snapshot-controls"><button className="secondary" disabled={busy} onClick={()=>void call("/"+job.id+"/cancel",undefined,"Could not cancel")}>Cancel staged upgrade</button></div>}
   </>}
   {job.status==="stopped"&&<p>{job.detail||"Stopped."} Nothing was flashed.</p>}
   {(job.status==="done"||job.status==="done_with_warnings"||job.status==="needs_attention")&&<>
    <p>{job.detail}</p>
    {checks.length>0&&<ul className="check-list">{checks.map(c=><li key={c.name}><span className={"drift-tag check-"+c.status}>{checkTag[c.status]??c.status}</span> <b>{c.name}</b> {c.detail}</li>)}</ul>}
    {unacked&&canAcknowledge&&<div className="snapshot-controls"><button className="secondary" disabled={busy} onClick={()=>void call("/"+job.id+"/acknowledge",undefined,"Could not acknowledge")}>Acknowledge result</button></div>}
   </>}
   {job.status==="preparing"&&canPrepare&&<div className="snapshot-controls"><button className="secondary" disabled={busy} onClick={()=>void call("/"+job.id+"/cancel",undefined,"Could not cancel")}>Cancel</button></div>}
  </div></div>}
 {!campaignMode&&!active&&!unacked&&(canPrepare?<div className="snapshot-controls"><button className="primary" disabled={busy||plan.blockers.length>0} onClick={()=>void call("",{version},"Could not start preparing")}>{busy?"Starting…":"Prepare upgrade to "+version}</button></div>:<div className="table-card"><p className="empty-line">Operators and administrators can prepare upgrades.</p></div>)}
 </div>;
}
