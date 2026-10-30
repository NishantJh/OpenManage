import { useEffect, useRef, useState } from "react";
import { Download, PackagePlus, RefreshCw, Trash2 } from "lucide-react";
import { apiFetch, apiJSON, csrfToken } from "../api";
import type { Device, PackageInstall, PackageReinstall, PackageSource, PackageUpdateCheck, Role } from "../types";

const kib=(b:number)=>b<1024*1024?(b/1024).toFixed(1)+" KiB":(b/1024/1024).toFixed(1)+" MiB";
const msg=(e:unknown,fallback:string)=>e instanceof Error?e.message:fallback;

// Third-party packages such as LuCI themes. An administrator registers a file (upload or a
// GitHub release link), reviews what it contains and its install scripts, and confirms by
// repeating its SHA-256. Only then can it be installed on a device, by typing the device name.
// Updates are looked for only when the check button is pressed, and a newer file is registered
// as a new source that goes through the same review.
export default function SoftwareSources({devices,role}:{devices:Device[];role:Role}){
 const admin=role==="admin",canRead=admin||role==="operator",enrolled=devices.filter(d=>d.status!=="unknown");
 const [sources,setSources]=useState<PackageSource[]>([]),[error,setError]=useState(""),[notice,setNotice]=useState(""),[busy,setBusy]=useState(false);
 const [link,setLink]=useState(""),[review,setReview]=useState<PackageSource|null>(null),[typedHash,setTypedHash]=useState("");
 const [updates,setUpdates]=useState<Record<number,PackageUpdateCheck>>({});
 const [deviceId,setDeviceId]=useState(""),[installs,setInstalls]=useState<PackageInstall[]>([]),[reinstalls,setReinstalls]=useState<PackageReinstall[]>([]);
 const [pick,setPick]=useState<PackageSource|null>(null),[typedName,setTypedName]=useState(""),[scriptsOk,setScriptsOk]=useState(false);
 const file=useRef<HTMLInputElement>(null),device=enrolled.find(d=>d.id===deviceId)??null,running=installs.some(i=>i.status==="running");
 useEffect(()=>{if(!device&&enrolled.length)setDeviceId(enrolled[0].id)},[enrolled.map(d=>d.id).join()]);
 async function loadSources(){if(!canRead)return;try{setSources(await apiJSON<PackageSource[]>("/api/package-sources","GET",undefined,"Could not load package sources"))}catch(e){setError(msg(e,"Could not load package sources"))}}
 useEffect(()=>{void loadSources()},[canRead]);
 async function loadDevice(){if(!deviceId||!canRead)return;const id=deviceId;try{const [i,r]=await Promise.all([apiJSON<PackageInstall[]>("/api/devices/"+id+"/package-installs","GET",undefined,"Could not load installs"),apiJSON<PackageReinstall[]>("/api/devices/"+id+"/package-reinstalls","GET",undefined,"Could not load the reinstall list")]);if(id===deviceId){setInstalls(i);setReinstalls(r)}}catch(e){setError(msg(e,"Could not load this device's packages"))}}
 useEffect(()=>{setInstalls([]);setReinstalls([]);setPick(null);setTypedName("");setScriptsOk(false);void loadDevice()},[deviceId]);
 useEffect(()=>{if(!running)return;const t=setInterval(()=>void loadDevice(),3000);return()=>clearInterval(t)},[running,deviceId]);
 async function run(fn:()=>Promise<void>,fail:string){setBusy(true);setError("");setNotice("");try{await fn()}catch(e){setError(msg(e,fail))}finally{setBusy(false)}}
 const register=(r:Promise<Response>)=>r.then(async res=>{const d=await res.json();if(!res.ok)throw Error(d.error||"Could not register the package");setReview(d as PackageSource);setTypedHash("");await loadSources()});
 const upload=()=>{const f=file.current?.files?.[0];if(!f)return;const form=new FormData();form.append("file",f);void run(async()=>{await register(fetchUpload(form));if(file.current)file.current.value=""},"Could not register the package")};
 const addLink=(url:string)=>void run(async()=>{await register(apiFetch("/api/package-sources","POST",{url}));setLink("")},"Could not register the package");
 const confirm=()=>review&&void run(async()=>{await apiJSON("/api/package-sources/"+review.id+"/confirm","POST",{sha256:typedHash.trim()},"Could not confirm the package");setNotice(review.name+" "+review.version+" is confirmed and can be installed.");setReview(null);await loadSources()},"Could not confirm the package");
 const remove=(s:PackageSource)=>{if(!window.confirm("Delete "+s.name+" "+s.version+" from the registered sources? Packages already on devices stay installed."))return;void run(async()=>{await apiJSON("/api/package-sources/"+s.id,"DELETE",undefined,"Could not delete the package source");if(review?.id===s.id)setReview(null);await loadSources()},"Could not delete the package source")};
 const check=(s:PackageSource)=>void run(async()=>{const c=await apiJSON<PackageUpdateCheck>("/api/package-sources/"+s.id+"/check-update","POST",undefined,"Could not check for an update");setUpdates(u=>({...u,[s.id]:c}))},"Could not check for an update");
 const install=()=>pick&&device&&void run(async()=>{await apiJSON("/api/devices/"+device.id+"/package-installs","POST",{source_id:pick.id,confirm_name:typedName,acknowledge_scripts:scriptsOk||pick.info.scripts.length===0},"Could not start the install");setPick(null);setTypedName("");setScriptsOk(false);await loadDevice()},"Could not start the install");
 if(!canRead)return null;
 const pending=sources.filter(s=>s.status==="pending"&&s.id!==review?.id),confirmed=sources.filter(s=>s.status==="confirmed");
 return <div className="package-sources"><div className="section-head"><div><h2>Software sources</h2><p>Packages that are not in the official feeds, such as LuCI themes. Nothing is installed until an administrator confirms the file and types the device name.</p></div></div>
 {error&&<div className="error backup-message">{error}</div>}{notice&&<div className="notice backup-message">{notice}</div>}
 {admin&&<div className="table-card"><div className="table-meta"><span>● &nbsp; Add a package</span><span>.ipk or .apk, up to 32 MiB</span></div><div className="upgrade-body">
  <div className="snapshot-controls"><input ref={file} type="file" accept=".ipk,.apk" aria-label="Package file"/><button className="secondary" disabled={busy} onClick={upload}><PackagePlus size={15}/> Upload</button></div>
  <div className="snapshot-controls"><input aria-label="GitHub release link" placeholder="https://github.com/owner/repo/releases/download/tag/file.apk" value={link} onChange={e=>setLink(e.target.value)} style={{minWidth:320}}/><button className="secondary" disabled={busy||!link.trim()} onClick={()=>addLink(link.trim())}><Download size={15}/> Download for review</button></div>
 </div></div>}
 {review&&<div className="table-card review-card"><div className="table-meta"><span>● &nbsp; Review {review.name} {review.version}</span><span>pending, expires after an hour</span></div><div className="upgrade-body">
  <p>{review.info.description||"No description."}</p>
  <p>{review.format.toUpperCase()} · {review.arch} · {kib(review.size)} · from <code>{review.origin}</code></p>
  <p>Depends on: {review.info.depends.length?review.info.depends.map(d=><code key={d}>{d} </code>):"nothing listed"}</p>
  <p>SHA-256 <code>{review.sha256}</code></p>
  {review.info.scripts.length>0?<div className="warning plan-warnings"><div>Install scripts run as root on every device this is installed on. Read them first.{review.info.scripts.map(s=><div key={s.name}><b>{s.name}</b><pre>{s.body}</pre></div>)}</div></div>:<p>This package has no install scripts.</p>}
  {admin?<div className="snapshot-controls"><input aria-label="Repeat the SHA-256 to confirm" placeholder="Repeat the SHA-256 above to confirm" value={typedHash} onChange={e=>setTypedHash(e.target.value)} style={{minWidth:320}}/><button className="primary" disabled={busy||typedHash.trim().toLowerCase()!==review.sha256} onClick={confirm}>Confirm package</button><button className="secondary" disabled={busy} onClick={()=>remove(review)}>Discard</button></div>:<p className="empty-line">Only an administrator can confirm.</p>}
 </div></div>}
 {admin&&pending.length>0&&<p className="firmware-read">{pending.length} more package{pending.length>1?"s":""} waiting for review: {pending.map(s=><button key={s.id} className="secondary" onClick={()=>{setReview(s);setTypedHash("")}}>{s.name} {s.version}</button>)}</p>}
 <div className="table-card"><div className="table-meta"><span>● &nbsp; Confirmed packages &nbsp; <b>{confirmed.length}</b></span><button className="secondary" onClick={()=>void loadSources()}><RefreshCw size={14}/></button></div>
  {confirmed.length===0?<p className="empty-line">None yet.{admin?" Add a package above.":""}</p>:<div className="table-scroll"><table><thead><tr><th>PACKAGE</th><th>VERSION</th><th>TYPE</th><th>FROM</th><th></th></tr></thead><tbody>{confirmed.map(s=>{const u=updates[s.id];return <tr key={s.id}><td>{s.name}</td><td><code>{s.version}</code></td><td>{s.format} · {s.arch}</td><td><small>{s.origin}</small>{u&&<small>{u.status==="up_to_date"?"This is the latest release ("+u.tag+").":"Newer release "+u.tag+" is available."}</small>}</td>
   <td>{admin&&<>{u?.status==="newer"&&u.url&&<button className="secondary" disabled={busy} onClick={()=>addLink(u.url!)}>Download {u.tag} for review</button>} {s.origin.startsWith("https://github.com/")&&<button className="secondary" disabled={busy} onClick={()=>check(s)}>Check for update</button>} <button className="secondary" disabled={busy||!device} onClick={()=>{setPick(s);setTypedName("");setScriptsOk(false)}}>Install…</button> <button className="row-action" aria-label={"Delete "+s.name} disabled={busy} onClick={()=>remove(s)}><Trash2 size={14}/></button></>}</td></tr>})}</tbody></table></div>}
 </div>
 <div className="section-head" style={{marginTop:24}}><div><h2>On a device</h2><p>Installs from the sources above, and third-party packages that need reinstalling after a flash.</p></div><div className="tools"><select aria-label="Device" className="secondary" value={deviceId} onChange={e=>setDeviceId(e.target.value)} disabled={!enrolled.length}>{enrolled.map(d=><option key={d.id} value={d.id}>{d.name}</option>)}</select></div></div>
 {pick&&device&&<div className="table-card"><div className="table-meta"><span>● &nbsp; Install {pick.name} {pick.version} on {device.name}</span></div><div className="upgrade-body">
  <p>The device is checked first (format, architecture, free space, and that nothing it already has would be replaced). The file's hash is verified on the device before it is installed.</p>
  {pick.info.scripts.length>0&&<label className="check-row"><input type="checkbox" checked={scriptsOk} onChange={e=>setScriptsOk(e.target.checked)}/> I read the install scripts. They run as root on {device.name}.</label>}
  <div className="snapshot-controls"><input aria-label="Device name" placeholder={"Type "+device.name+" to confirm"} value={typedName} onChange={e=>setTypedName(e.target.value)}/><button className="primary" disabled={busy||running||typedName!==device.name||(pick.info.scripts.length>0&&!scriptsOk)} onClick={install}>Install</button><button className="secondary" onClick={()=>setPick(null)}>Cancel</button></div>
 </div></div>}
 {reinstalls.length>0&&<div className="table-card"><div className="table-meta"><span>● &nbsp; Needs reinstalling &nbsp; <b>{reinstalls.length}</b></span><span>nothing is reinstalled automatically</span></div><div className="table-scroll"><table><thead><tr><th>PACKAGE</th><th>CONFIRMED SOURCE</th><th></th></tr></thead><tbody>{reinstalls.map(r=>{const src=confirmed.find(s=>s.id===r.source_id);return <tr key={r.name}><td>{r.name}</td><td>{src?<>{src.name} <code>{src.version}</code></>:<small>{r.reason||"No confirmed source fits this device."}</small>}</td><td>{admin&&src&&<button className="secondary" disabled={busy||running} onClick={()=>{setPick(src);setTypedName("");setScriptsOk(false)}}>Install…</button>}</td></tr>})}</tbody></table></div></div>}
 <div className="table-card"><div className="table-meta"><span>● &nbsp; Installs on {device?.name??"this device"} &nbsp; <b>{installs.length}</b></span></div>
  {installs.length===0?<p className="empty-line">No packages installed from a source yet.</p>:<div className="upgrade-body">{installs.map(i=><div key={i.id}><p><b>{i.name} {i.version}</b> · <span className={"job-"+i.status}>{i.status==="running"?(i.step||"working").toUpperCase():i.status.toUpperCase()}</span> · {new Date(i.created_at).toLocaleString()}{i.detail&&" · "+i.detail}</p>{i.status!=="running"&&i.checks.length>0&&<ul className="check-list">{i.checks.map(c=><li key={c.name}><span className={"drift-tag check-"+c.status}>{c.status.toUpperCase()}</span> <b>{c.name}</b> {c.detail}</li>)}</ul>}</div>)}</div>}
 </div></div>;
}

function fetchUpload(form:FormData){return fetch("/api/package-sources",{method:"POST",headers:{"X-CSRF-Token":csrfToken()},body:form})}
