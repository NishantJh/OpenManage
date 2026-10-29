import { useEffect, useRef, useState } from "react";
import { CloudDownload } from "lucide-react";
import { apiJSON } from "../api";
import type { FirmwareInventory, FirmwarePlan, FirmwareRelease, FirmwareSettings, Role } from "../types";

function Names({title,items,empty,note}:{title:string;items:string[];empty:string;note?:string}){
 return <div className="table-card firmware-list"><div className="table-meta"><span>● &nbsp; {title} &nbsp; <b>{items.length}</b></span>{note&&<span>{note}</span>}</div><div className="firmware-items">{items.length?items.map(i=><code key={i}>{i}</code>):<p className="empty-line">{empty}</p>}</div></div>;
}

const releaseLabel=(r:FirmwareRelease,current:string)=>r.version+(r.version===current?" (installed)":r.snapshot?" (snapshot)":r.release_candidate?" (release candidate)":"");

// Picks the newest final release, or the installed one when nothing newer exists.
function defaultRelease(releases:FirmwareRelease[],current:string){
 return releases.find(r=>!r.snapshot&&!r.release_candidate)?.version??current;
}

// What upgrading one device to a chosen release would request. Read-only: it reads
// release metadata from the build server and changes nothing on the device.
export default function UpgradePlan({deviceId,inventory,role,settings,onSettings}:{deviceId:string;inventory:FirmwareInventory;role:Role;settings:FirmwareSettings|null;onSettings:(s:FirmwareSettings)=>void}){
 const [releases,setReleases]=useState<FirmwareRelease[]>([]),[version,setVersion]=useState(""),[plan,setPlan]=useState<FirmwarePlan|null>(null),[busy,setBusy]=useState(false),[error,setError]=useState("");
 const current=inventory.manifest.release,canPlan=role==="admin"||role==="operator",enabled=Boolean(settings?.build_server_enabled),key=useRef("");
 useEffect(()=>{if(!enabled||!canPlan)return;setError("");apiJSON<{releases:FirmwareRelease[]}>("/api/firmware/releases","GET",undefined,"Could not list releases").then(d=>{setReleases(d.releases);setVersion(v=>v&&d.releases.some(r=>r.version===v)?v:defaultRelease(d.releases,current))}).catch(e=>setError(e instanceof Error?e.message:"Could not list releases"))},[enabled,canPlan,deviceId]);
 useEffect(()=>{if(!version||!enabled||!canPlan)return;const k=deviceId+"|"+version+"|"+inventory.id;key.current=k;setBusy(true);setError("");setPlan(null);apiJSON<{plan:FirmwarePlan}>("/api/devices/"+deviceId+"/firmware-plan?version="+encodeURIComponent(version),"GET",undefined,"Could not prepare the upgrade plan").then(d=>{if(key.current===k)setPlan(d.plan)}).catch(e=>{if(key.current===k)setError(e instanceof Error?e.message:"Could not prepare the upgrade plan")}).finally(()=>{if(key.current===k)setBusy(false)})},[version,deviceId,inventory.id,enabled]);
 async function turnOn(){setBusy(true);setError("");try{onSettings(await apiJSON<FirmwareSettings>("/api/firmware/settings","PUT",{build_server_enabled:true,build_server_url:settings?.build_server_url},"Could not turn on the build server"))}catch(e){setError(e instanceof Error?e.message:"Could not turn on the build server")}finally{setBusy(false)}}
 const host=(()=>{try{return new URL(settings?.build_server_url??"").host}catch{return "the build server"}})();
 return <div className="upgrade-plan"><div className="section-head"><div><h2>Upgrade plan</h2><p>Pick any release to see what an upgrade would keep, rename or leave out. Nothing is built or installed yet.</p></div>{enabled&&canPlan&&releases.length>0&&<div className="tools"><select aria-label="Release" className="secondary" value={version} onChange={e=>setVersion(e.target.value)}>{releases.map(r=><option key={r.version} value={r.version}>{releaseLabel(r,current)}</option>)}</select></div>}</div>
 {!enabled?<div className="info-card blue"><CloudDownload size={20}/><div><b>Build server is off</b><p>Upgrade plans use the OpenWrt build server at <b>{host}</b>, which lists the releases and the packages each one offers. Turning it on lets OpenManage ask it about each device's target and model. Your package list is sent only when an image is built, which is a later step.</p>{role==="admin"?<div className="snapshot-controls"><button className="primary" onClick={()=>void turnOn()} disabled={busy}>{busy?"Turning on…":"Turn on the build server"}</button></div>:<p>Ask an administrator to turn it on.</p>}{error&&<p>{error}</p>}<span>OPT-IN</span></div></div>
 :!canPlan?<div className="table-card"><p className="empty-line">Operators and administrators can view upgrade plans.</p></div>
 :<>
  {error&&<div className="error backup-message">{error}</div>}
  {busy&&!plan&&<div className="table-card"><p className="empty-line">Asking the build server…</p></div>}
  {plan&&<>
   <div className="firmware-facts table-card">
    <div><small>UPGRADE</small><b>{current} → {plan.version}</b><span>{plan.version_code||"revision not reported"}</span></div>
    <div><small>IMAGE PROFILE</small><b>{plan.profile||"—"}</b><span>{plan.profile_title||"no matching image"}</span></div>
    <div><small>PACKAGE MANAGER</small><b>{plan.package_manager}</b><span>{plan.kernel?"kernel "+plan.kernel:""}</span></div>
   </div>
   {plan.blockers.map(b=><div key={b} className="error backup-message">{b}</div>)}
   {plan.warnings.length>0&&<div className="warning plan-warnings"><ul>{plan.warnings.map(w=><li key={w}>{w}</li>)}</ul></div>}
   {plan.blockers.length===0&&<div className="firmware-grid">
    <Names title="Your packages in the new image" items={plan.packages.filter(p=>!p.startsWith("-"))} empty="None besides the release's defaults." note="on top of the defaults"/>
    <Names title="Not in the official feeds" items={plan.unavailable} empty="None. Every package you added is available." note="install again afterwards"/>
    <Names title="Defaults left out" items={plan.removed_defaults} empty="None." note="you removed these"/>
    {plan.renamed.length>0&&<Names title="Renamed in this release" items={plan.renamed.map(r=>r.from+" → "+r.to)} empty=""/>}
    {plan.dropped.length>0&&<Names title="Dropped in this release" items={plan.dropped} empty=""/>}
   </div>}
  </>}
 </>}
 </div>;
}
