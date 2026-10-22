import { useState } from "react";
import type { FormEvent } from "react";
import { KeyRound } from "lucide-react";
import { apiJSON } from "../api";

// One modal for both password flows: a user changing their own password (needs the
// current one) and an administrator resetting someone else's.
export default function PasswordModal({target,onClose,onDone}:{target:{id:string;username:string}|null;onClose:()=>void;onDone:(message:string)=>void}){
 const own=target===null;
 const [current,setCurrent]=useState(""),[next,setNext]=useState(""),[confirm,setConfirm]=useState(""),[busy,setBusy]=useState(false),[error,setError]=useState("");
 async function submit(e:FormEvent){e.preventDefault();setError("");if(next!==confirm){setError("The new passwords do not match.");return}setBusy(true);try{if(own){await apiJSON("/api/auth/password","POST",{current_password:current,new_password:next},"Could not change your password");onDone("Password changed. Your other sessions were signed out.")}else{await apiJSON("/api/users/"+target.id+"/password","POST",{password:next},"Could not reset the password");onDone("Password reset for "+target.username+". Their sessions were signed out.")}}catch(e){setError(e instanceof Error?e.message:"Could not change the password")}finally{setBusy(false);setCurrent("")}}
 return <div className="overlay" onMouseDown={e=>{if(e.target===e.currentTarget)onClose()}}><form className="modal" onSubmit={submit}><div className="modal-head"><div><div className="eyebrow">ACCESS</div><h2>{own?"Change your password":"Reset password"}</h2><p>{own?"Other sessions for your account will be signed out.":"Sets a new password for "+target.username+" and signs them out."}</p></div><button type="button" className="close" onClick={onClose}>×</button></div>
 {own&&<label>Current password<input type="password" autoComplete="current-password" required autoFocus value={current} onChange={e=>setCurrent(e.target.value)}/></label>}
 <label>New password<input type="password" autoComplete="new-password" required minLength={12} maxLength={72} autoFocus={!own} value={next} onChange={e=>setNext(e.target.value)}/><small className="field-help">12 to 72 characters.</small></label>
 <label>Confirm new password<input type="password" autoComplete="new-password" required value={confirm} onChange={e=>setConfirm(e.target.value)}/></label>
 {error&&<div className="error">{error}</div>}
 <div className="modal-actions"><button type="button" className="secondary" onClick={onClose}>Cancel</button><button className="primary" disabled={busy}>{busy?"Saving…":<><KeyRound size={15}/> {own?"Change password":"Reset password"}</>}</button></div></form></div>;
}
