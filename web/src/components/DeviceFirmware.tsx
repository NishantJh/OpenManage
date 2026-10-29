import { useEffect, useRef, useState } from "react";
import { PackageSearch, RefreshCw } from "lucide-react";
import { apiJSON } from "../api";
import type { Device, FirmwareInventory, FirmwareInventoryResult, Role } from "../types";

const space=(kb:number)=>kb<1024?kb+" KiB":kb<1024*1024?(kb/1024).toFixed(kb<10240?1:0)+" MiB":(kb/1024/1024).toFixed(1)+" GiB";

function List({title,items,empty,note}:{title:string;items:string[];empty:string;note?:string}){
 return <div className="table-card firmware-list"><div className="table-meta"><span>● &nbsp; {title} &nbsp; <b>{items.length}</b></span>{note&&<span>{note}</span>}</div><div className="firmware-items">{items.length?items.map(i=><code key={i}>{i}</code>):<p className="empty-line">{empty}</p>}</div></div>;
}

// Firmware and package inventory of one device at a time. Reading it is read-only on
// the device; operators and administrators can read it again, everyone can view it.
export default function DeviceFirmware({devices,role,onChanged}:{devices:Device[];role:Role;onChanged:()=>void}){
 const enrolled=devices.filter(d=>d.status!=="unknown");
 const [deviceId,setDeviceId]=useState(""),[inventory,setInventory]=useState<FirmwareInventory|null>(null),[loading,setLoading]=useState(false),[busy,setBusy]=useState(false),[showAll,setShowAll]=useState(false),[message,setMessage]=useState<{text:string;tone:"notice"|"error"}|null>(null);
 const current=useRef(""),device=enrolled.find(d=>d.id===deviceId)??null,canRead=role==="admin"||role==="operator";
 useEffect(()=>{if(!device&&enrolled.length)setDeviceId(enrolled[0].id)},[enrolled.map(d=>d.id).join()]);
 async function load(){if(!deviceId)return;const id=deviceId;setLoading(true);try{const data=await apiJSON<{inventory:FirmwareInventory|null}>("/api/devices/"+id+"/firmware-inventory","GET",undefined,"Could not load the firmware inventory");if(current.current===id)setInventory(data.inventory)}catch(e){if(current.current===id)setMessage({tone:"error",text:e instanceof Error?e.message:"Could not load the firmware inventory"})}finally{if(current.current===id)setLoading(false)}}
 useEffect(()=>{current.current=deviceId;setInventory(null);setMessage(null);setBusy(false);setShowAll(false);void load()},[deviceId]);
 async function read(){const id=deviceId;setBusy(true);setMessage(null);try{const data=await apiJSON<FirmwareInventoryResult>("/api/devices/"+id+"/firmware-inventory","POST",undefined,"Could not read the device");if(current.current!==id)return;setInventory(data.inventory);setMessage({tone:"notice",text:data.first?"Inventory stored.":data.changed?"The device changed since the last read; the new inventory is stored.":"Nothing changed since the last read."});onChanged()}catch(e){if(current.current===id)setMessage({tone:"error",text:e instanceof Error?e.message:"Could not read the device"})}finally{if(current.current===id)setBusy(false)}}
 const m=inventory?.manifest;
 return <div className="device-firmware"><div className="section-head"><div><h2>Firmware and packages</h2><p>What each device runs today. Reading it changes nothing on the device. Upgrades come in a later step.</p></div><div className="tools"><select aria-label="Device" className="secondary" value={deviceId} onChange={e=>setDeviceId(e.target.value)} disabled={!enrolled.length}>{enrolled.map(d=><option key={d.id} value={d.id}>{d.name}</option>)}</select>{canRead&&<button className="primary" onClick={()=>void read()} disabled={busy||!deviceId}>{busy?"Reading…":<><RefreshCw size={15}/> Read from device</>}</button>}</div></div>
 {message&&<div className={message.tone+" backup-message"}>{message.text}</div>}
 {!enrolled.length?<div className="table-card"><p className="empty-line">Enroll a device to see its firmware.</p></div>:loading?<div className="table-card"><p className="empty-line">Loading…</p></div>:!m?<div className="table-card"><p className="empty-line"><PackageSearch size={15}/> No inventory yet.{canRead?" Read from device to see its release and packages.":""}</p></div>:<>
  <div className="firmware-facts table-card">
   <div><small>RELEASE</small><b>OpenWrt {m.release}</b><span>{m.revision}</span></div>
   <div><small>DEVICE</small><b>{m.model||device?.model||"—"}</b><span>{m.board_name||"board name not reported"}</span></div>
   <div><small>TARGET</small><b>{m.target||"—"}</b><span>{m.arch}</span></div>
   <div><small>PACKAGE MANAGER</small><b>{m.package_manager}</b><span>{m.packages.length} packages installed</span></div>
   <div><small>FREE FLASH</small><b>{space(m.overlay_free_kb)}</b><span>of {space(m.overlay_total_kb)}</span></div>
   <div><small>FREE MEMORY</small><b>{space(m.mem_available_kb)}</b><span>of {space(m.mem_total_kb)}</span></div>
  </div>
  <p className="firmware-read">Read {new Date(inventory!.captured_at).toLocaleString()} · last matched {new Date(inventory!.last_verified_at).toLocaleString()}</p>
  {m.extroot&&<div className="error">This device keeps its packages on external storage (extroot). Firmware upgrades will not be offered for it.</div>}
  {!m.rom_known&&<div className="notice">This device has no read-only firmware image to compare with, so packages you added and defaults you removed cannot be told apart from the image.</div>}
  <div className="firmware-grid">
   <List title="Installed by you" items={m.user_installed} empty="None. Everything on the device came with the firmware image." note="kept on upgrade"/>
   <List title="Removed from the image" items={m.removed_defaults} empty={m.rom_known?"None.":"Unknown on this device."} note="left out on upgrade"/>
   <List title="Custom feeds" items={m.custom_feeds} empty="None." note="credentials hidden"/>
   <List title="Files kept on upgrade" items={m.keep_list} empty="Only the standard configuration files." note="/etc/sysupgrade.conf"/>
  </div>
  <div className="table-card firmware-all"><div className="table-meta"><span>● &nbsp; All packages &nbsp; <b>{m.packages.length}</b></span><button className="secondary" onClick={()=>setShowAll(v=>!v)}>{showAll?"Hide":"Show"}</button></div>{showAll&&<div className="table-scroll"><table><thead><tr><th>PACKAGE</th><th>VERSION</th><th></th></tr></thead><tbody>{m.packages.map(p=><tr key={p.name}><td>{p.name}</td><td><code>{p.version||"—"}</code></td><td>{p.user_installed&&<span className="drift-tag">YOURS</span>}{p.hold&&<span className="drift-tag">HELD</span>}</td></tr>)}</tbody></table></div>}</div>
 </>}
 </div>;
}
