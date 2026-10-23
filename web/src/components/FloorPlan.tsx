import { useEffect, useRef, useState } from "react";
import { Eye, EyeOff, Plus, Save, Trash2 } from "lucide-react";
import { apiFetch, apiJSON } from "../api";
import type { Device, FloorPlan as Plan, FloorRoom, MonitoringSettings, Role, WifiClient, WifiClientEvent } from "../types";

const STEP=0.5,MAX=500,REFRESH_MS=60000;
const GREEN="#1a9e55",RED="#d64545";
const pollChoices:[number,string][]=[[60,"Every minute (roaming tests)"],[300,"Every 5 minutes"],[600,"Every 10 minutes"]];
const kindText={connect:"connected to",roam:"roamed",disconnect:"left"} as const;
const ago=(iso:string)=>{const m=Math.max(0,Math.round((Date.now()-new Date(iso).getTime())/60000));return m<1?"just now":m<60?m+" min ago":Math.round(m/60)+" h ago"};
const snap=(v:number)=>Math.round(v/STEP)*STEP;
const clamp=(v:number,hi:number)=>Math.min(Math.max(v,0),hi);
const inRoom=(r:FloorRoom,x:number,y:number)=>x>=r.x&&x<=r.x+r.w&&y>=r.y&&y<=r.y+r.h;
// A client's dot goes in the room that holds the AP it is associated with (the nearest
// room if the AP sits outside every room), spread around the AP so dots do not stack.
export function dotPosition(rooms:FloorRoom[],ap:{x:number;y:number},index:number,count:number){
 const room=rooms.find(r=>inRoom(r,ap.x,ap.y))??[...rooms].sort((a,b)=>Math.hypot(a.x+a.w/2-ap.x,a.y+a.h/2-ap.y)-Math.hypot(b.x+b.w/2-ap.x,b.y+b.h/2-ap.y))[0];
 const angle=count>1?2*Math.PI*index/count-Math.PI/2:Math.PI/2,radius=count>1?0.9:0.8;
 const x=ap.x+Math.cos(angle)*radius,y=ap.y+Math.sin(angle)*radius;
 const within=(v:number,lo:number,size:number)=>Math.min(Math.max(v,lo+0.3),lo+size-0.3);
 return room?{x:within(x,room.x,room.w),y:within(y,room.y,room.h),room:room.name}:{x,y,room:""};
}

// Rooms and AP positions in metres. Drawing only: nothing here touches a router.
export default function FloorPlan({devices,role,onChanged}:{devices:Device[];role:Role;onChanged:()=>void}){
 const canEdit=role!=="readonly";
 const [plan,setPlan]=useState<Plan>({rooms:[],placements:[]}),[dirty,setDirty]=useState(false),[saving,setSaving]=useState(false),[message,setMessage]=useState<{text:string;tone:"notice"|"error"}|null>(null);
 const [drag,setDrag]=useState<{kind:"room"|"ap";key:string;dx:number;dy:number}|null>(null),[pick,setPick]=useState("");
 const svg=useRef<SVGSVGElement>(null);
 const [settings,setSettings]=useState<MonitoringSettings|null>(null),[clients,setClients]=useState<WifiClient[]>([]),[events,setEvents]=useState<WifiClientEvent[]>([]),[trackBusy,setTrackBusy]=useState(false);
 const isAdmin=role==="admin",tracking=Boolean(settings?.client_tracking_enabled);
 async function loadClients(){try{const [c,e]=await Promise.all([apiFetch("/api/wifi-clients"),apiFetch("/api/wifi-client-events?limit=20")]);if(c.ok)setClients(await c.json());if(e.ok)setEvents(await e.json())}catch{/* the map still works without client data */}}
 async function loadSettings(){try{const r=await apiFetch("/api/monitoring/settings");if(r.ok)setSettings(await r.json())}catch{/* shown as tracking off */}}
 useEffect(()=>{void loadSettings();void loadClients();const t=setInterval(()=>void loadClients(),REFRESH_MS);return()=>clearInterval(t)},[]);
 // Settings are read again just before saving so a monitoring change made elsewhere is not overwritten.
 async function saveTracking(enabled:boolean,interval:number){setTrackBusy(true);setMessage(null);try{const cur=await apiJSON<MonitoringSettings>("/api/monitoring/settings","GET",undefined,"Could not read monitoring settings");const data=await apiJSON<MonitoringSettings>("/api/monitoring/settings","PUT",{enabled:cur.enabled,interval_seconds:cur.interval_seconds,client_tracking_enabled:enabled,client_poll_interval_seconds:interval},"Could not save client tracking");setSettings(data);setMessage({tone:"notice",text:data.client_tracking_enabled?"Client tracking is on. Clients appear after the next poll.":"Client tracking is off. Data already collected stays until it expires or is cleared."})}catch(err){setMessage({tone:"error",text:err instanceof Error?err.message:"Could not save client tracking"})}finally{setTrackBusy(false)}}
 async function patchClient(c:WifiClient,change:{label?:string;hidden?:boolean}){setMessage(null);try{const r=await apiFetch("/api/wifi-clients/"+c.id,"PATCH",change);if(!r.ok)throw Error();setClients(prev=>prev.map(x=>x.id===c.id?{...x,...change}:x))}catch{setMessage({tone:"error",text:"Could not update the device."})}}
 async function clearClients(){if(!window.confirm("Delete every tracked device, its last location and its roaming history?"))return;try{const r=await apiFetch("/api/wifi-clients","DELETE");if(!r.ok)throw Error();setClients([]);setEvents([]);setMessage({tone:"notice",text:"Client data cleared."});onChanged()}catch{setMessage({tone:"error",text:"Could not clear client data."})}}
 useEffect(()=>{(async()=>{try{const r=await apiFetch("/api/floor-plan");if(!r.ok)throw Error();setPlan(await r.json())}catch{setMessage({tone:"error",text:"Could not load the floor plan."})}})()},[]);
 const extentX=Math.max(12,...plan.rooms.map(r=>r.x+r.w+1)),extentY=Math.max(8,...plan.rooms.map(r=>r.y+r.h+1));
 const change=(next:Plan)=>{setPlan(next);setDirty(true)};
 const nameOf=(id:string)=>devices.find(d=>d.id===id)?.name??"Unknown device";
 function toPlan(e:{clientX:number;clientY:number}){const s=svg.current!,r=s.getBoundingClientRect();return {x:(e.clientX-r.left)/r.width*extentX,y:(e.clientY-r.top)/r.height*extentY}}
 function start(e:React.PointerEvent,kind:"room"|"ap",key:string,ox:number,oy:number){if(!canEdit)return;const p=toPlan(e);(e.currentTarget as Element).setPointerCapture(e.pointerId);setDrag({kind,key,dx:p.x-ox,dy:p.y-oy})}
 function move(e:React.PointerEvent){if(!drag)return;const p=toPlan(e);
  if(drag.kind==="room")change({...plan,rooms:plan.rooms.map(r=>r.id===drag.key?{...r,x:clamp(snap(p.x-drag.dx),MAX-r.w),y:clamp(snap(p.y-drag.dy),MAX-r.h)}:r)});
  else change({...plan,placements:plan.placements.map(a=>a.device_id===drag.key?{...a,x:clamp(snap(p.x-drag.dx),MAX),y:clamp(snap(p.y-drag.dy),MAX)}:a)})}
 function addRoom(){const n=plan.rooms.length+1;change({...plan,rooms:[...plan.rooms,{id:"r"+Date.now().toString(36),name:"Room "+n,x:0,y:0,w:4,h:3}]})}
 function patchRoom(id:string,p:Partial<FloorRoom>){change({...plan,rooms:plan.rooms.map(r=>r.id===id?{...r,...p}:r)})}
 function place(){if(!pick)return;change({...plan,placements:[...plan.placements,{device_id:pick,x:1,y:1}]});setPick("")}
 async function save(){setSaving(true);setMessage(null);try{const saved=await apiJSON<Plan>("/api/floor-plan","PUT",{rooms:plan.rooms,placements:plan.placements},"Could not save the floor plan");setPlan(saved);setDirty(false);setMessage({tone:"notice",text:"Floor plan saved."});onChanged()}catch(err){setMessage({tone:"error",text:err instanceof Error?err.message:"Could not save the floor plan"})}finally{setSaving(false)}}
 const apOf=(id:string)=>plan.placements.find(a=>a.device_id===id);
 const shown=clients.filter(c=>!c.hidden&&apOf(c.device_id)),perAp=new Map<string,WifiClient[]>();
 shown.forEach(c=>perAp.set(c.device_id,[...(perAp.get(c.device_id)??[]),c]));
 const dots=[...perAp.entries()].flatMap(([ap,list])=>list.map((c,i)=>({c,...dotPosition(plan.rooms,apOf(ap)!,i,list.length)})));
 const unplaced=devices.filter(d=>!plan.placements.some(a=>a.device_id===d.id));
 return <div className="audit-section"><div className="section-head"><div><h2>Floor plan</h2><p>Draw your rooms and drag each access point to where it sits. Metres; drawing only, nothing is changed on any router.</p></div><div className="tools">{canEdit&&<><button className="secondary" onClick={addRoom}><Plus size={15}/> Add room</button><button className="primary" disabled={!dirty||saving} onClick={()=>void save()}><Save size={15}/> {saving?"Saving…":"Save"}</button></>}</div></div>
 {message&&<div className={message.tone}>{message.text}<button onClick={()=>setMessage(null)}>Dismiss</button></div>}
 <div className="table-meta"><span>Device tracking: <b>{tracking?"on":"off"}</b>{tracking&&settings?" · "+(pollChoices.find(([s])=>s===settings.client_poll_interval_seconds)?.[1]??settings.client_poll_interval_seconds+" s").toLowerCase():""}</span>{isAdmin&&settings&&<span><select aria-label="Client poll interval" className="secondary" value={settings.client_poll_interval_seconds} disabled={trackBusy} onChange={e=>void saveTracking(tracking,Number(e.target.value))}>{pollChoices.map(([s,l])=><option key={s} value={s}>{l}</option>)}</select> <button className="secondary" disabled={trackBusy} onClick={()=>void saveTracking(!tracking,settings.client_poll_interval_seconds)}>{tracking?"Turn off":"Turn on"}</button></span>}{!tracking&&<span>Off by default. When on, each AP is asked which devices are connected, and devices not seen for 24 h disappear from the map.</span>}</div>
 <div className="table-card"><svg ref={svg} viewBox={`0 0 ${extentX} ${extentY}`} style={{width:"100%",maxHeight:480,touchAction:"none",background:"rgba(127,127,127,.1)"}} onPointerMove={move} onPointerUp={()=>setDrag(null)} role="img" aria-label="Floor plan">
  {plan.rooms.map(r=><g key={r.id}><rect x={r.x} y={r.y} width={r.w} height={r.h} fill="rgba(80,120,220,.08)" stroke="#5078dc" strokeWidth={0.06} onPointerDown={e=>start(e,"room",r.id,r.x,r.y)} style={{cursor:canEdit?"move":"default"}}/><text x={r.x+0.2} y={r.y+0.6} fontSize={0.5} fill="currentColor" pointerEvents="none">{r.name}</text></g>)}
  {plan.placements.map(a=><g key={a.device_id} onPointerDown={e=>start(e,"ap",a.device_id,a.x,a.y)} style={{cursor:canEdit?"grab":"default"}}><circle cx={a.x} cy={a.y} r={0.35} fill="#1f6feb" stroke="#fff" strokeWidth={0.08}/><text x={a.x+0.5} y={a.y+0.15} fontSize={0.45} fill="currentColor" pointerEvents="none">{nameOf(a.device_id)}</text></g>)}
  {dots.map(({c,x,y,room})=><circle key={c.id} cx={x} cy={y} r={0.28} fill={c.connected?GREEN:"#fff"} stroke={c.connected?"#fff":RED} strokeWidth={c.connected?0.08:0.12}><title>{(c.label||"Unknown device")+" ("+c.mac_hint+") · "+(c.connected?"connected":"not connected, last seen "+ago(c.last_seen))+" · "+nameOf(c.device_id)+(room?" · "+room:"")+(c.signal_dbm!==null&&c.connected?" · "+c.signal_dbm+" dBm":"")}</title></circle>)}
 </svg>
 <div className="table-meta"><span><svg width="12" height="12" aria-hidden="true"><circle cx="6" cy="6" r="5" fill={GREEN}/></svg> Connected</span><span><svg width="12" height="12" aria-hidden="true"><circle cx="6" cy="6" r="4.5" fill="#fff" stroke={RED} strokeWidth="2"/></svg> Not connected (seen in the last 24 h)</span><span>Position is the room of the AP the device is connected to, not a measured location.</span></div></div>
 {canEdit&&<div className="table-card"><div className="table-scroll"><table><thead><tr><th>Room</th><th>Width (m)</th><th>Height (m)</th><th/></tr></thead><tbody>
  {plan.rooms.map(r=><tr key={r.id}><td><input maxLength={60} value={r.name} onChange={e=>patchRoom(r.id,{name:e.target.value})}/></td><td><input type="number" min={0.5} max={100} step={STEP} value={r.w} onChange={e=>patchRoom(r.id,{w:Math.max(0.5,Number(e.target.value)||0.5)})}/></td><td><input type="number" min={0.5} max={100} step={STEP} value={r.h} onChange={e=>patchRoom(r.id,{h:Math.max(0.5,Number(e.target.value)||0.5)})}/></td><td><button className="secondary" aria-label={"Remove "+r.name} onClick={()=>change({...plan,rooms:plan.rooms.filter(x=>x.id!==r.id)})}><Trash2 size={15}/></button></td></tr>)}
  {plan.placements.map(a=><tr key={a.device_id}><td colSpan={3}>Access point: <b>{nameOf(a.device_id)}</b></td><td><button className="secondary" aria-label={"Unplace "+nameOf(a.device_id)} onClick={()=>change({...plan,placements:plan.placements.filter(x=>x.device_id!==a.device_id)})}><Trash2 size={15}/></button></td></tr>)}
  <tr><td colSpan={3}><select className="secondary" value={pick} onChange={e=>setPick(e.target.value)}><option value="">Place a device…</option>{unplaced.map(d=><option key={d.id} value={d.id}>{d.name}</option>)}</select></td><td><button className="secondary" disabled={!pick} onClick={place}>Place</button></td></tr>
 </tbody></table></div></div>} {(clients.length>0||events.length>0)&&<div className="table-card"><div className="table-meta"><span>Devices seen in the last 24 h <b>{clients.length}</b></span>{isAdmin&&<button className="secondary" onClick={()=>void clearClients()}><Trash2 size={15}/> Clear client data</button>}</div><div className="table-scroll"><table><thead><tr><th>Device</th><th>Status</th><th>Access point</th><th>Signal</th><th>Last seen</th><th/></tr></thead><tbody>
  {clients.map(c=><tr key={c.id}><td>{canEdit?<input aria-label={"Name for "+c.mac_hint} maxLength={60} defaultValue={c.label} placeholder="Unknown device" onBlur={e=>{const v=e.target.value.trim();if(v!==c.label)void patchClient(c,{label:v})}}/>:<b>{c.label||"Unknown device"}</b>}<small> {c.mac_hint}</small></td><td>{c.connected?"Connected":"Not connected"}</td><td>{nameOf(c.device_id)} ({c.interface})</td><td>{c.connected&&c.signal_dbm!==null?c.signal_dbm+" dBm":"–"}</td><td>{ago(c.last_seen)}</td><td>{canEdit&&<button className="secondary" aria-label={(c.hidden?"Show ":"Hide ")+(c.label||c.mac_hint)+" on the map"} onClick={()=>void patchClient(c,{hidden:!c.hidden})}>{c.hidden?<Eye size={15}/>:<EyeOff size={15}/>}</button>}</td></tr>)}
 </tbody></table></div>
 {events.length>0&&<div className="table-scroll"><table><thead><tr><th>Roaming history</th><th>When</th><th>Signal</th></tr></thead><tbody>{events.map(e=><tr key={e.id}><td>{e.label||"Unknown device"} <small>{e.mac_hint}</small> {kindText[e.kind]} {e.kind==="disconnect"?(e.from_device_id?nameOf(e.from_device_id):"the network"):e.kind==="roam"?(e.from_device_id?"from "+nameOf(e.from_device_id)+" ":"")+"to "+(e.to_device_id?nameOf(e.to_device_id):"another AP"):(e.to_device_id?nameOf(e.to_device_id):"the network")}</td><td>{ago(e.at)}</td><td>{e.signal_dbm!==null?e.signal_dbm+" dBm":"–"}</td></tr>)}</tbody></table></div>}</div>}
 </div>;
}
