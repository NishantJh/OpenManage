import { AlertTriangle, ShieldCheck, Wifi } from "lucide-react";
import { useState } from "react";
import type { FormEvent } from "react";
import { apiFetch } from "../api";
import type { Device } from "../types";
import { enrollGuidance, looksEnrollable } from "../guidance";

type Err={message:string;code?:string};
const insecurePage=typeof location!=="undefined"&&location.protocol==="http:"&&!["localhost","127.0.0.1","[::1]"].includes(location.hostname);
const wizardGuidance:Record<string,string>={
 ...enrollGuidance,
 ssh_password_rejected:"Check the username and the router's root password. It is the password you use to sign in to LuCI.",
 no_root_password:"The router has no root password, so anyone on the network could sign in. Set one in LuCI (System → Administration), then try again.",
 key_install_failed:"The router accepted the password but would not store the key. Check that /etc/dropbear is writable and has free space.",
 host_key_mismatch:"The router's identity changed between the two steps. Start again, and only continue if you trust the network you are on.",
};

export default function AddApWizard({onClose,onAdded,onAdvanced}:{onClose:()=>void;onAdded:(d:Device)=>void;onAdvanced:()=>void}){
 const [step,setStep]=useState<1|2>(1),[address,setAddress]=useState(""),[fingerprint,setFingerprint]=useState(""),[name,setName]=useState(""),[role,setRole]=useState<"access_point"|"router">("access_point"),[password,setPassword]=useState(""),[busy,setBusy]=useState(false),[error,setError]=useState<Err|null>(null);
 async function post(path:string,body:unknown){const r=await apiFetch(path,"POST",body);const data=await r.json().catch(()=>({}));return {ok:r.ok,data}}
 async function find(e:FormEvent){e.preventDefault();setBusy(true);setError(null);try{const {ok,data}=await post("/api/device-bootstrap/probe",{address:address.trim()});if(!ok){setError({message:data.error||"Could not reach the device",code:data.code});return}setFingerprint(data.host_key_fingerprint);setStep(2)}catch{setError({message:"Could not reach the OpenManage API."})}finally{setBusy(false)}}
 async function add(e:FormEvent){e.preventDefault();setBusy(true);setError(null);try{const {ok,data}=await post("/api/device-bootstrap",{name:name.trim(),address:address.trim(),role,username:"root",password,expected_fingerprint:fingerprint});if(!ok){setError({message:data.error||"Could not add the device",code:data.code});return}setPassword("");onAdded(data as Device)}catch{setError({message:"Could not reach the OpenManage API."})}finally{setBusy(false);setPassword("")}}
 return <div className="overlay" onMouseDown={e=>{if(e.target===e.currentTarget)onClose()}}><form className="modal enroll-modal" onSubmit={step===1?find:add}>
  <div className="modal-head"><div><div className="eyebrow">ADD ACCESS POINT · STEP {step} OF 2</div><h2>Add an access point</h2><p>{step===1?"Enter the device's IP address. OpenManage will look up its identity first.":"Confirm the identity, then give the root password once."}</p></div><button type="button" className="close" onClick={onClose}>×</button></div>
  <div className="warning" style={{marginBottom:15,borderColor:"#2d5141",background:"#16271f",color:"#91d8b5"}}><ShieldCheck size={15}/><span><b>Your root password is used once and never stored.</b> OpenManage signs in one time to install its own key, then forgets the password. It is not saved, logged or written to the audit trail. The password stays valid on the router, so you can turn off SSH password login there afterwards.</span></div>
  {step===1&&<><label>Device IP address<input required autoFocus value={address} onChange={e=>setAddress(e.target.value)} placeholder="192.168.1.2" maxLength={45}/>{address.trim()&&!looksEnrollable(address)&&<small className="field-help">Only private IP addresses are accepted (10.x, 172.16–31.x, 192.168.x).</small>}</label></>}
  {step===2&&<><div className="enroll-target"><b>Router identity (host key)</b><code style={{wordBreak:"break-all"}}>{fingerprint}</code><small>Found at {address.trim()}. If this network could be tampered with, compare this with the fingerprint shown by your router first. Every later connection must present exactly this key.</small></div>
   <label>Name (optional)<input value={name} onChange={e=>setName(e.target.value)} placeholder="e.g. Hall access point" maxLength={128}/></label>
   <div style={{display:"flex",alignItems:"center",gap:12,marginBottom:15}}><span style={{fontSize:11,fontWeight:600,color:"#bfc3cf"}}>Role</span><div className="fp-seg" role="group" aria-label="Device role">{([["access_point","Access point"],["router","Router / gateway"]] as const).map(([v,l])=><button type="button" key={v} className={role===v?"on":""} onClick={()=>setRole(v)}>{l}</button>)}</div></div>
   <label>Router root password<input required type="password" autoComplete="off" autoFocus value={password} onChange={e=>setPassword(e.target.value)} maxLength={512}/><small className="field-help">Used once, then discarded.</small></label>
   {insecurePage&&<div className="warning" style={{marginBottom:15}}><AlertTriangle size={15}/> This page uses plain HTTP, so the password would cross the network unencrypted. Use HTTPS before entering it.</div>}</>}
  {error&&<div className="error enroll-error" role="alert"><AlertTriangle size={17}/><div><b>{error.message}</b>{error.code&&wizardGuidance[error.code]&&<p>{wizardGuidance[error.code]}</p>}</div></div>}
  <div className="modal-actions" style={{justifyContent:"space-between"}}><button type="button" className="secondary" onClick={onAdvanced}>Advanced / manual</button><span style={{display:"flex",gap:9}}>{step===2&&<button type="button" className="secondary" onClick={()=>{setStep(1);setError(null);setPassword("")}}>Back</button>}<button className="primary" disabled={busy}>{busy?(step===1?"Looking up…":"Adding…"):step===1?"Find device":<><Wifi size={15}/> Add and connect</>}</button></span></div>
 </form></div>;
}
