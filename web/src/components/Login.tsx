import { useState } from "react";
import type { FormEvent } from "react";
import { Router, LockKeyhole } from "lucide-react";
import { apiJSON } from "../api";

export default function Login({onLogin}:{onLogin:(username:string,role:string)=>void}){
 const [username,setUsername]=useState(""),[password,setPassword]=useState(""),[error,setError]=useState(""),[busy,setBusy]=useState(false);
 async function submit(e:FormEvent){e.preventDefault();setBusy(true);setError("");try{const data=await apiJSON<{user:{username:string;role:string}}>("/api/auth/login","POST",{username,password},"Sign-in failed",{csrf:false});onLogin(data.user.username as string,data.user.role as string)}catch(e){setError(e instanceof Error?e.message:"Sign-in failed")}finally{setBusy(false)}}
 return <div className="auth-screen"><form className="auth-card" onSubmit={submit}><div className="auth-mark"><Router size={23}/></div><div className="eyebrow">NETWORK CONTROL PLANE</div><h1>Sign in to OpenManage</h1><p>Use your local administrator account to manage your network.</p>{error&&<div className="auth-error">{error}</div>}<label>Username<input autoFocus autoComplete="username" required value={username} onChange={e=>setUsername(e.target.value)}/></label><label>Password<input type="password" autoComplete="current-password" required value={password} onChange={e=>setPassword(e.target.value)}/></label><button className="primary auth-submit" disabled={busy}>{busy?"Signing in…":<><LockKeyhole size={15}/> Sign in</>}</button><small>Sessions expire after 12 hours. Keep this service on a trusted network.</small></form></div>
}
