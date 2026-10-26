import { useState } from "react";
import type { FormEvent } from "react";
import { RotateCcw } from "lucide-react";
import { apiFetch } from "../api";
import type { Device, DeviceBackup, DeviceRestore } from "../types";

// Restoring replaces the device's configuration and reboots it. The server enforces the
// real rules (same device and board, OpenManage's own access preserved); this dialog makes
// the consequences plain and asks for the device name before anything is sent.
export default function RestoreModal({device,backup,onClose,onStarted}:{device:Device;backup:DeviceBackup;onClose:()=>void;onStarted:(r:DeviceRestore)=>void}){
 const [confirmName,setConfirmName]=useState(""),[acceptRelease,setAcceptRelease]=useState(false),[needRelease,setNeedRelease]=useState(false),[busy,setBusy]=useState(false),[error,setError]=useState("");
 async function submit(e:FormEvent){e.preventDefault();setBusy(true);setError("");try{const r=await apiFetch("/api/devices/"+device.id+"/backups/"+backup.id+"/restore","POST",{confirm_name:confirmName,accept_release_mismatch:acceptRelease});const data=await r.json().catch(()=>({}));if(!r.ok){if(data.code==="release_mismatch"){setNeedRelease(true);return}throw Error(data.error||"Could not start the restore")}onStarted(data as DeviceRestore)}catch(e){setError(e instanceof Error?e.message:"Could not start the restore")}finally{setBusy(false)}}
 const ready=confirmName===device.name&&(!needRelease||acceptRelease);
 return <div className="overlay" onMouseDown={e=>{if(e.target===e.currentTarget)onClose()}}><form className="modal restore-modal" onSubmit={submit}><div className="modal-head"><div><div className="eyebrow">RESTORE</div><h2>Restore this backup</h2><p>{device.name} · backup from {new Date(backup.captured_at).toLocaleString()}{backup.release?" · "+backup.release:""}</p></div><button type="button" className="close" onClick={onClose}>×</button></div>
 <div className="warning backup-warning"><ul>
  <li>The device's configuration is replaced by this backup and the device <b>reboots</b>. It is offline for a few minutes.</li>
  <li>The device's root password, SSH keys and Wi-Fi passphrases go back to what they were at that time. An older backup may still hold a password or key you have since changed.</li>
  <li>If the backup has a different IP address or SSH port, the device may come back where OpenManage cannot reach it.</li>
  <li>OpenManage refuses a backup that would lock it out of the device.</li>
 </ul></div>
 {needRelease&&<div className="warning backup-warning">This backup was taken on a different OpenWrt release{backup.release?" ("+backup.release+")":""} than the device is running{device.firmware?" ("+device.firmware+")":""}. Old settings can misbehave on a newer release.<label className="check"><input type="checkbox" checked={acceptRelease} onChange={e=>setAcceptRelease(e.target.checked)}/> I understand and want to restore anyway</label></div>}
 <label className="backup-field">Type the device name <code>{device.name}</code> to confirm<input autoFocus required autoComplete="off" value={confirmName} onChange={e=>setConfirmName(e.target.value)}/></label>
 {error&&!needRelease&&<div className="error">{error}</div>}
 <div className="modal-actions"><button type="button" className="secondary" onClick={onClose}>Cancel</button><button className="primary danger" disabled={busy||!ready}>{busy?"Starting…":<><RotateCcw size={15}/> Restore and reboot</>}</button></div></form></div>;
}
