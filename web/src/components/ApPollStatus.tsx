import { useEffect, useState } from "react";
import { apiFetch } from "../api";
import type { Device } from "../types";

type PollStatus={device_id:string;polled_at:string;ok:boolean;error?:string;clients:number};

// Lists access points whose latest Wi-Fi poll failed, so an AP with no devices on the map is
// explained instead of silently empty. The status lives in server memory and refills after the
// next poll; it shows nothing while every AP is read fine.
export default function ApPollStatus({devices,tracking}:{devices:Device[];tracking:boolean}){
 const [status,setStatus]=useState<PollStatus[]>([]);
 useEffect(()=>{if(!tracking)return;let live=true;const load=async()=>{try{const r=await apiFetch("/api/wifi-clients/status");if(r.ok&&live)setStatus(await r.json())}catch{/* the map works without it */}};void load();const t=setInterval(()=>void load(),60000);return()=>{live=false;clearInterval(t)}},[tracking]);
 const bad=status.filter(s=>!s.ok);
 if(!tracking||bad.length===0)return null;
 const name=(id:string)=>devices.find(d=>d.id===id)?.name??"Unknown access point";
 return <ul className="fp-poll-problems" aria-label="Access points that could not be read">{bad.map(s=><li key={s.device_id}><b>{name(s.device_id)}</b> could not be read: {s.error||"unknown problem"}. Its devices are not updating.</li>)}</ul>
}
