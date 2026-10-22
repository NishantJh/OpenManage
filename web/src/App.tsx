import { useEffect, useState } from "react";
import { Router } from "lucide-react";
import { apiFetch } from "./api";
import Dashboard from "./components/Dashboard";
import Login from "./components/Login";

export default function App(){
 const [username,setUsername]=useState<string|null>(null),[role,setRole]=useState<string|null>(null),[checking,setChecking]=useState(true);
 useEffect(()=>{let alive=true;apiFetch("/api/auth/me").then(async r=>{if(!r.ok)throw Error("not signed in");return r.json()}).then(data=>{if(alive){setUsername(data.user.username as string);setRole(data.user.role as string)}}).catch(()=>{if(alive)setUsername(null)}).finally(()=>{if(alive)setChecking(false)});return()=>{alive=false}},[]);
 async function logout(){try{await apiFetch("/api/auth/logout","POST")}finally{setUsername(null);setRole(null)}}
 if(checking)return <div className="auth-screen"><div className="auth-card"><div className="brand-icon"><Router size={21}/></div><h1>OpenManage</h1><p>Checking your session…</p></div></div>;
 if(!username)return <Login onLogin={(name,userRole)=>{setUsername(name);setRole(userRole)}}/>;
 return <Dashboard username={username} role={role??"readonly"} onLogout={logout}/>;
}
