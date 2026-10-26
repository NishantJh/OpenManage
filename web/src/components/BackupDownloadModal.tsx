import { useState } from "react";
import type { FormEvent } from "react";
import { Download } from "lucide-react";
import { apiFetch } from "../api";
import type { Device, DeviceBackup } from "../types";

// A backup archive is root-equivalent for the device (it holds its password hashes, keys and
// Wi-Fi passphrases), so the administrator re-enters their password for every download.
// The password travels in the POST body only.
export default function BackupDownloadModal({device,backup,onClose,onDone}:{device:Device;backup:DeviceBackup;onClose:()=>void;onDone:(message:string)=>void}){
 const [password,setPassword]=useState(""),[busy,setBusy]=useState(false),[error,setError]=useState("");
 const insecure=window.location.protocol==="http:"&&!["localhost","127.0.0.1","[::1]"].includes(window.location.hostname);
 async function submit(e:FormEvent){e.preventDefault();setBusy(true);setError("");try{const r=await apiFetch("/api/devices/"+device.id+"/backups/"+backup.id+"/download","POST",{password});if(!r.ok){const data=await r.json().catch(()=>({}));throw Error(data.error||"Could not download the backup")}
  const url=URL.createObjectURL(await r.blob()),a=document.createElement("a");a.href=url;a.download="openmanage-backup-"+device.name.replace(/[^A-Za-z0-9._-]+/g,"_")+"-"+backup.captured_at.slice(0,10)+".tar.gz";document.body.appendChild(a);a.click();a.remove();URL.revokeObjectURL(url);onDone("Backup downloaded. Keep the file private: anyone holding it can read this device's passwords and keys.")}catch(e){setError(e instanceof Error?e.message:"Could not download the backup")}finally{setBusy(false);setPassword("")}}
 return <div className="overlay" onMouseDown={e=>{if(e.target===e.currentTarget)onClose()}}><form className="modal" onSubmit={submit}><div className="modal-head"><div><div className="eyebrow">BACKUP</div><h2>Download backup</h2><p>{device.name} · {new Date(backup.captured_at).toLocaleString()}</p></div><button type="button" className="close" onClick={onClose}>×</button></div>
 <div className="warning">This file contains the device's root password hash, SSH keys and Wi-Fi passphrases. Store it like a password manager export, and do not email or share it.</div>
 {insecure&&<div className="warning backup-warning">You are on plain HTTP, so the file and your password cross the network unencrypted. Use HTTPS or a trusted network before downloading.</div>}
 <label className="backup-field">Your password<input type="password" autoComplete="current-password" required autoFocus value={password} onChange={e=>setPassword(e.target.value)}/><small className="field-help">The download is recorded in the audit log.</small></label>
 {error&&<div className="error">{error}</div>}
 <div className="modal-actions"><button type="button" className="secondary" onClick={onClose}>Cancel</button><button className="primary" disabled={busy||!password}>{busy?"Preparing…":<><Download size={15}/> Download</>}</button></div></form></div>;
}
