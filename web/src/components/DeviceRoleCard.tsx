import { useState } from "react";
import { apiJSON } from "../api";
import type { Device } from "../types";

// What this device is for. It only changes where OpenManage reads device names from;
// nothing is written to the device.
export default function DeviceRoleCard({device,onChanged}:{device:Device;onChanged:()=>void}){
 const [role,setRole]=useState(device.role),[dhcp,setDhcp]=useState(device.dhcp_host),[busy,setBusy]=useState(false),[error,setError]=useState("");
 async function save(next:{role?:string;dhcp_host?:boolean}){setBusy(true);setError("");try{const d=await apiJSON<Device>("/api/devices/"+device.id,"PATCH",next,"Could not save the device role");setRole(d.role);setDhcp(d.dhcp_host);onChanged()}catch(e){setError(e instanceof Error?e.message:"Could not save the device role")}finally{setBusy(false)}}
 return <div className="fp-card" style={{padding:14,margin:"14px 0"}}>
  <div style={{display:"flex",alignItems:"center",gap:14,flexWrap:"wrap"}}>
   <b style={{fontSize:12}}>Device role</b>
   <div className="fp-seg" role="group" aria-label="Device role">{([["access_point","Access point"],["router","Router / gateway"]] as const).map(([v,l])=><button type="button" key={v} className={role===v?"on":""} disabled={busy} onClick={()=>role!==v&&void save({role:v})}>{l}</button>)}</div>
   <button type="button" role="switch" aria-checked={dhcp} aria-label="DHCP host" className={"fp-switch"+(dhcp?" on":"")} disabled={busy} onClick={()=>void save({dhcp_host:!dhcp})}><span/></button>
   <span style={{fontSize:11}}>DHCP host</span>
  </div>
  <p style={{fontSize:10,color:"#83899a",margin:"8px 0 0"}}>Mark the device that runs DHCP so its lease file names devices on the floor plan. Other devices' lease files are ignored. Devices without a DHCP host are named by reverse DNS when possible.</p>
  {error&&<p className="health-error">{error}</p>}
 </div>;
}
