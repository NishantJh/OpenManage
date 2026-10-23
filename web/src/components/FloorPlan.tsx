import { useEffect, useRef, useState } from "react";
import { Eye, EyeOff, Plus, Radio, Save, Trash2, Wifi } from "lucide-react";
import { apiFetch, apiJSON } from "../api";
import type { Device, FloorPlan as Plan, FloorRoom, MonitoringSettings, Role, WifiClient, WifiClientEvent } from "../types";

const STEP=0.5,MAX=500,MIN_SIZE=0.5,MAX_SIZE=100,REFRESH_MS=60000;
const pollChoices:[number,string][]=[[60,"1 min"],[300,"5 min"],[600,"10 min"]];
const ago=(iso:string)=>{const m=Math.max(0,Math.round((Date.now()-new Date(iso).getTime())/60000));return m<1?"just now":m<60?m+" min ago":Math.round(m/60)+" h ago"};
const snap=(v:number)=>Math.round(v/STEP)*STEP;
const clamp=(v:number,hi:number)=>Math.min(Math.max(v,0),hi);
const inRoom=(r:FloorRoom,x:number,y:number)=>x>=r.x&&x<=r.x+r.w&&y>=r.y&&y<=r.y+r.h;
const bars=(dbm:number|null)=>dbm===null?0:dbm>-55?4:dbm>-65?3:dbm>-75?2:1;

// A metres field that can be typed into freely (empty or half-typed values are kept as
// text) and only changes the plan once the text is a valid size.
function SizeInput({label,value,onCommit}:{label:string;value:number;onCommit:(v:number)=>void}){
 const [text,setText]=useState(String(value)),[focused,setFocused]=useState(false);
 useEffect(()=>{if(!focused)setText(String(value))},[value,focused]);
 return <input aria-label={label} type="number" inputMode="decimal" min={MIN_SIZE} max={MAX_SIZE} step={STEP} value={text} onFocus={()=>setFocused(true)} onBlur={()=>setFocused(false)} onChange={e=>{setText(e.target.value);const v=Number(e.target.value);if(e.target.value!==""&&v>=MIN_SIZE&&v<=MAX_SIZE)onCommit(v)}}/>;
}

const WALL_SNAP=0.8,PAD=1.4; // PAD leaves room around the plan for APs on outer walls and their labels
// Access points hang on walls, so a dragged AP snaps to the nearest wall of the room it is
// in (or next to) when it is within WALL_SNAP metres of it.
export function snapToWall(rooms:FloorRoom[],x:number,y:number){
 let best:{x:number;y:number;d:number}|null=null;
 for(const r of rooms){
  const cx=Math.min(Math.max(x,r.x),r.x+r.w),cy=Math.min(Math.max(y,r.y),r.y+r.h);
  const edges=[{x:r.x,y:cy,d:Math.abs(x-r.x)},{x:r.x+r.w,y:cy,d:Math.abs(x-r.x-r.w)},{x:cx,y:r.y,d:Math.abs(y-r.y)},{x:cx,y:r.y+r.h,d:Math.abs(y-r.y-r.h)}];
  const near=x>=r.x-WALL_SNAP&&x<=r.x+r.w+WALL_SNAP&&y>=r.y-WALL_SNAP&&y<=r.y+r.h+WALL_SNAP;
  if(!near)continue;
  for(const e of edges)if(e.d<=WALL_SNAP&&(!best||e.d<best.d))best=e;
 }
 return best?{x:best.x,y:best.y}:{x,y};
}

// A client's dot goes in the room that holds the AP it is associated with (the nearest
// room if the AP sits outside every room), spread around the AP so dots do not stack.
export function dotPosition(rooms:FloorRoom[],ap:{x:number;y:number},index:number,count:number){
 const room=rooms.find(r=>inRoom(r,ap.x,ap.y))??[...rooms].sort((a,b)=>Math.hypot(a.x+a.w/2-ap.x,a.y+a.h/2-ap.y)-Math.hypot(b.x+b.w/2-ap.x,b.y+b.h/2-ap.y))[0];
 // Dots fan out in an arc facing into the room (away from the wall the AP hangs on).
 const toward=room?Math.atan2(room.y+room.h/2-ap.y,room.x+room.w/2-ap.x):Math.PI/2,spread=Math.min(Math.PI*0.9,0.55*(count-1));
 const angle=count>1?toward-spread/2+spread*index/(count-1):toward,radius=count>1?1.15:0.95;
 const x=ap.x+Math.cos(angle)*radius,y=ap.y+Math.sin(angle)*radius;
 const within=(v:number,lo:number,size:number)=>Math.min(Math.max(v,lo+0.3),lo+size-0.3);
 return room?{x:within(x,room.x,room.w),y:within(y,room.y,room.h),room:room.name}:{x,y,room:""};
}

// Rooms and AP positions in metres, with the devices connected to each AP drawn on top.
// Drawing only: nothing here touches a router.
export default function FloorPlan({devices,role,onChanged}:{devices:Device[];role:Role;onChanged:()=>void}){
 const canEdit=role!=="readonly",isAdmin=role==="admin";
 const [plan,setPlan]=useState<Plan>({rooms:[],placements:[]}),[dirty,setDirty]=useState(false),[saving,setSaving]=useState(false),[message,setMessage]=useState<{text:string;tone:"notice"|"error"}|null>(null);
 const [drag,setDrag]=useState<{kind:"room"|"ap"|"size";key:string;dx:number;dy:number}|null>(null),[pick,setPick]=useState("");
 const [settings,setSettings]=useState<MonitoringSettings|null>(null),[clients,setClients]=useState<WifiClient[]>([]),[events,setEvents]=useState<WifiClientEvent[]>([]),[trackBusy,setTrackBusy]=useState(false);
 const svg=useRef<SVGSVGElement>(null);
 const tracking=Boolean(settings?.client_tracking_enabled);

 async function loadClients(){try{const [c,e]=await Promise.all([apiFetch("/api/wifi-clients"),apiFetch("/api/wifi-client-events?limit=20")]);if(c.ok)setClients(await c.json());if(e.ok)setEvents(await e.json())}catch{/* the map still works without client data */}}
 async function loadSettings(){try{const r=await apiFetch("/api/monitoring/settings");if(r.ok)setSettings(await r.json())}catch{/* shown as tracking off */}}
 useEffect(()=>{(async()=>{try{const r=await apiFetch("/api/floor-plan");if(!r.ok)throw Error();setPlan(await r.json())}catch{setMessage({tone:"error",text:"Could not load the floor plan."})}})();void loadSettings();void loadClients();const t=setInterval(()=>void loadClients(),REFRESH_MS);return()=>clearInterval(t)},[]);

 // Settings are read again just before saving so a monitoring change made elsewhere is not overwritten.
 async function saveTracking(enabled:boolean,interval:number){setTrackBusy(true);setMessage(null);try{const cur=await apiJSON<MonitoringSettings>("/api/monitoring/settings","GET",undefined,"Could not read monitoring settings");const data=await apiJSON<MonitoringSettings>("/api/monitoring/settings","PUT",{enabled:cur.enabled,interval_seconds:cur.interval_seconds,client_tracking_enabled:enabled,client_poll_interval_seconds:interval},"Could not save device tracking");setSettings(data);setMessage({tone:"notice",text:data.client_tracking_enabled?"Device tracking is on. Devices appear after the next poll.":"Device tracking is off. Data already collected stays until it expires or is cleared."})}catch(err){setMessage({tone:"error",text:err instanceof Error?err.message:"Could not save device tracking"})}finally{setTrackBusy(false)}}
 async function patchClient(c:WifiClient,change:{label?:string;hidden?:boolean}){setMessage(null);try{const r=await apiFetch("/api/wifi-clients/"+c.id,"PATCH",change);if(!r.ok)throw Error();setClients(prev=>prev.map(x=>x.id===c.id?{...x,...change}:x))}catch{setMessage({tone:"error",text:"Could not update the device."})}}
 async function clearClients(){if(!window.confirm("Delete every tracked device, its last location and its roaming history?"))return;try{const r=await apiFetch("/api/wifi-clients","DELETE");if(!r.ok)throw Error();setClients([]);setEvents([]);setMessage({tone:"notice",text:"Device data cleared."});onChanged()}catch{setMessage({tone:"error",text:"Could not clear device data."})}}

 const nameOf=(id:string)=>devices.find(d=>d.id===id)?.name??"Unknown access point";
 const extentX=Math.max(12,...plan.rooms.map(r=>r.x+r.w+1)),extentY=Math.max(7,...plan.rooms.map(r=>r.y+r.h+1));
 const change=(next:Plan)=>{setPlan(next);setDirty(true)};
 function toPlan(e:{clientX:number;clientY:number}){const r=svg.current!.getBoundingClientRect();return {x:(e.clientX-r.left)/r.width*(extentX+PAD)-PAD,y:(e.clientY-r.top)/r.height*(extentY+PAD)-PAD}}
 function start(e:React.PointerEvent,kind:"room"|"ap"|"size",key:string,ox:number,oy:number){if(!canEdit)return;const p=toPlan(e);(e.currentTarget as Element).setPointerCapture(e.pointerId);setDrag({kind,key,dx:p.x-ox,dy:p.y-oy})}
 function move(e:React.PointerEvent){if(!drag)return;const p=toPlan(e);
  if(drag.kind==="size")change({...plan,rooms:plan.rooms.map(r=>r.id===drag.key?{...r,w:Math.min(MAX_SIZE,Math.max(MIN_SIZE,snap(p.x-r.x))),h:Math.min(MAX_SIZE,Math.max(MIN_SIZE,snap(p.y-r.y)))}:r)});
  else if(drag.kind==="room")change({...plan,rooms:plan.rooms.map(r=>r.id===drag.key?{...r,x:clamp(snap(p.x-drag.dx),MAX-r.w),y:clamp(snap(p.y-drag.dy),MAX-r.h)}:r)});
  else{const pos=snapToWall(plan.rooms,clamp(snap(p.x-drag.dx),MAX),clamp(snap(p.y-drag.dy),MAX));change({...plan,placements:plan.placements.map(a=>a.device_id===drag.key?{...a,...pos}:a)})}}
 function addRoom(){const n=plan.rooms.length+1;change({...plan,rooms:[...plan.rooms,{id:"r"+Date.now().toString(36),name:"Room "+n,x:0,y:0,w:4,h:3}]})}
 function patchRoom(id:string,p:Partial<FloorRoom>){change({...plan,rooms:plan.rooms.map(r=>r.id===id?{...r,...p}:r)})}
 function place(){if(!pick)return;const r=plan.rooms[0],pos=r?{x:r.x+r.w/2,y:r.y}:{x:1,y:1};change({...plan,placements:[...plan.placements,{device_id:pick,...pos}]});setPick("")}
 async function save(){setSaving(true);setMessage(null);try{const saved=await apiJSON<Plan>("/api/floor-plan","PUT",{rooms:plan.rooms,placements:plan.placements},"Could not save the floor plan");setPlan(saved);setDirty(false);setMessage({tone:"notice",text:"Floor plan saved."});onChanged()}catch(err){setMessage({tone:"error",text:err instanceof Error?err.message:"Could not save the floor plan"})}finally{setSaving(false)}}

 const apOf=(id:string)=>plan.placements.find(a=>a.device_id===id);
 const onMap=clients.filter(c=>!c.hidden&&apOf(c.device_id)),perAp=new Map<string,WifiClient[]>();
 onMap.forEach(c=>perAp.set(c.device_id,[...(perAp.get(c.device_id)??[]),c]));
 const dots=[...perAp.entries()].flatMap(([ap,list])=>list.map((c,i)=>({c,...dotPosition(plan.rooms,apOf(ap)!,i,list.length)})));
 const unplaced=devices.filter(d=>!plan.placements.some(a=>a.device_id===d.id));
 const connectedCount=clients.filter(c=>c.connected).length;
 const eventText=(e:WifiClientEvent)=>e.kind==="roam"?"roamed"+(e.from_device_id?" from "+nameOf(e.from_device_id):"")+" to "+(e.to_device_id?nameOf(e.to_device_id):"another AP"):e.kind==="connect"?"joined "+(e.to_device_id?nameOf(e.to_device_id):"the network"):"left "+(e.from_device_id?nameOf(e.from_device_id):"the network");

 return <section className="audit-section fp">
  <div className="section-head"><div><h2>Floor plan</h2><p>Where your access points are and which devices are connected to them. Metres; drawing only, nothing is changed on any router.</p></div>
   {canEdit&&<div className="tools"><button className="secondary" onClick={addRoom}><Plus size={15}/> Add room</button><button className="primary" disabled={!dirty||saving} onClick={()=>void save()}><Save size={15}/> {saving?"Saving…":"Save"}</button></div>}</div>
  {message&&<div className={message.tone}>{message.text}<button onClick={()=>setMessage(null)}>Dismiss</button></div>}
  <div className="fp-grid">
   <div className="fp-card fp-map">
    <div className="fp-toolbar">
     <div className="fp-legend"><span className="fp-chip"><i className="fp-dot on"/>Connected</span><span className="fp-chip"><i className="fp-dot off"/>Seen in the last 24 h</span><span className="fp-chip"><i className="fp-dot ap"/>Access point</span></div>
     <div className="fp-track">
      <button type="button" role="switch" aria-checked={tracking} aria-label="Device tracking" className={"fp-switch"+(tracking?" on":"")} disabled={!isAdmin||!settings||trackBusy} onClick={()=>settings&&void saveTracking(!tracking,settings.client_poll_interval_seconds)}><span/></button>
      <span className="fp-track-label">{tracking?"Tracking devices":"Tracking off"}</span>
      {isAdmin&&settings&&tracking&&<div className="fp-seg" role="group" aria-label="Poll interval">{pollChoices.map(([s,l])=><button type="button" key={s} className={settings.client_poll_interval_seconds===s?"on":""} disabled={trackBusy} onClick={()=>void saveTracking(true,s)}>{l}</button>)}</div>}
     </div>
    </div>
    <svg ref={svg} viewBox={`${-PAD} ${-PAD} ${extentX+PAD} ${extentY+PAD}`} className="fp-svg" onPointerMove={move} onPointerUp={()=>setDrag(null)} role="img" aria-label="Floor plan">
     <defs>
      <pattern id="fp-grid" width="1" height="1" patternUnits="userSpaceOnUse"><path d="M1 0H0V1" fill="none" stroke="rgba(255,255,255,.045)" strokeWidth="0.03"/></pattern>
      <radialGradient id="fp-coverage"><stop offset="0" stopColor="#8d78f5" stopOpacity=".28"/><stop offset="1" stopColor="#8d78f5" stopOpacity="0"/></radialGradient>
     </defs>
     <rect x={-PAD} y={-PAD} width={extentX+PAD} height={extentY+PAD} fill="url(#fp-grid)"/>
     {plan.rooms.map(r=><g key={r.id}>
      <rect x={r.x} y={r.y} width={r.w} height={r.h} rx={0.18} className="fp-room" onPointerDown={e=>start(e,"room",r.id,r.x,r.y)} style={{cursor:canEdit?"move":"default"}}/>
      <text x={r.x+0.35} y={r.y+0.75} className="fp-room-name">{r.name.toUpperCase()}</text>
      <text x={r.x+0.35} y={r.y+r.h-0.3} className="fp-room-size">{r.w} × {r.h} m</text>
      {canEdit&&<circle cx={r.x+r.w} cy={r.y+r.h} r={0.3} className="fp-handle" onPointerDown={e=>start(e,"size",r.id,0,0)}><title>Drag to resize</title></circle>}</g>)}
     {plan.placements.map(a=><g key={a.device_id} onPointerDown={e=>start(e,"ap",a.device_id,a.x,a.y)} style={{cursor:canEdit?"grab":"default"}}>
      <circle cx={a.x} cy={a.y} r={2.4} fill="url(#fp-coverage)" pointerEvents="none"/>
      <circle cx={a.x} cy={a.y} r={0.42} className="fp-ap"/><circle cx={a.x} cy={a.y} r={0.14} fill="#0c0f15" pointerEvents="none"/>
      <text x={a.x} y={a.y-0.7} textAnchor="middle" className="fp-ap-name" pointerEvents="none">{nameOf(a.device_id)}</text></g>)}
     {dots.map(({c,x,y,room})=><g key={c.id}>
      {c.connected&&<circle cx={x} cy={y} r={0.5} className="fp-halo"/>}
      <circle cx={x} cy={y} r={0.26} className={c.connected?"fp-client on":"fp-client off"}><title>{(c.label||c.name||"Unknown device")+" ("+c.mac_hint+") · "+(c.connected?"connected":"not connected, last seen "+ago(c.last_seen))+" · "+nameOf(c.device_id)+(room?" · "+room:"")+(c.signal_dbm!==null&&c.connected?" · "+c.signal_dbm+" dBm":"")}</title></circle>
      {(c.label||c.name)&&<text x={x+0.42} y={y+0.12} className="fp-client-name" pointerEvents="none">{c.label||c.name}</text>}</g>)}
    </svg>
    {plan.rooms.length===0&&<div className="fp-empty">{canEdit?"Add a room to start drawing your floor plan.":"No floor plan has been drawn yet."}</div>}
    <p className="fp-note">A device is shown in the room of the access point it is connected to. This is not a measured position.</p>
   </div>
   <aside className="fp-card fp-side">
    <div className="fp-side-head"><div><h3>Devices</h3><small>{clients.length===0?(tracking?"Waiting for the first poll":"Tracking is off"):connectedCount+" connected · "+clients.length+" seen in 24 h"}</small></div>{isAdmin&&clients.length>0&&<button className="secondary fp-icon" aria-label="Clear device data" title="Clear device data" onClick={()=>void clearClients()}><Trash2 size={15}/></button>}</div>
    {clients.length===0&&<div className="fp-blank"><Wifi size={22}/><p>{tracking?"Connected devices will appear here after the next poll.":"Turn on tracking to see which devices are connected to which access point, and where they roamed."}</p></div>}
    <ul className="fp-devices">{clients.map(c=><li key={c.id} className={c.hidden?"hidden":""}>
     <i className={"fp-dot "+(c.connected?"on":"off")} title={c.connected?"Connected":"Not connected"}/>
     <div className="fp-device-main">{canEdit?<input aria-label={"Name for "+c.mac_hint} maxLength={60} defaultValue={c.label} placeholder={c.name||"Unknown device"} onBlur={e=>{const v=e.target.value.trim();if(v!==c.label)void patchClient(c,{label:v})}}/>:<b>{c.label||c.name||"Unknown device"}</b>}
      <small>{c.connected?nameOf(c.device_id):"Last on "+nameOf(c.device_id)} · {c.mac_hint}</small></div>
     <div className="fp-device-meta">{c.connected&&<span className="fp-bars" title={c.signal_dbm!==null?c.signal_dbm+" dBm":"No signal reading"}>{[1,2,3,4].map(n=><i key={n} className={n<=bars(c.signal_dbm)?"on":""} style={{height:3+n*3}}/>)}</span>}<small>{c.connected?"now":ago(c.last_seen)}</small></div>
     {canEdit&&<button className="secondary fp-icon" aria-label={(c.hidden?"Show ":"Hide ")+(c.label||c.name||c.mac_hint)+" on the map"} title={c.hidden?"Show on map":"Hide from map"} onClick={()=>void patchClient(c,{hidden:!c.hidden})}>{c.hidden?<Eye size={14}/>:<EyeOff size={14}/>}</button>}
    </li>)}</ul>
    {events.length>0&&<><h3 className="fp-sub">Roaming history</h3><ul className="fp-timeline">{events.map(e=><li key={e.id} className={e.kind}><i/><div><span><b>{e.label||"Unknown device"}</b> {eventText(e)}</span><small>{ago(e.at)}{e.signal_dbm!==null?" · "+e.signal_dbm+" dBm":""}</small></div></li>)}</ul></>}
   </aside>
  </div>
  {canEdit&&<details className="fp-card fp-edit"><summary><Radio size={15}/> Edit rooms and access points</summary>
   <div className="fp-edit-grid">
    {plan.rooms.length>0&&<div className="fp-edit-head"><span>Room</span><span>Width (m)</span><span>Height (m)</span><span/></div>}
    {plan.rooms.map(r=><div className="fp-edit-row" key={r.id}><input aria-label="Room name" maxLength={60} value={r.name} onChange={e=>patchRoom(r.id,{name:e.target.value})}/><SizeInput label={r.name+" width"} value={r.w} onCommit={w=>patchRoom(r.id,{w})}/><SizeInput label={r.name+" height"} value={r.h} onCommit={h=>patchRoom(r.id,{h})}/><button className="secondary fp-icon" aria-label={"Remove "+r.name} onClick={()=>change({...plan,rooms:plan.rooms.filter(x=>x.id!==r.id)})}><Trash2 size={14}/></button></div>)}
    {plan.placements.map(a=><div className="fp-edit-row fp-edit-ap" key={a.device_id}><span><i className="fp-dot ap"/> Access point <b>{nameOf(a.device_id)}</b></span><span/><span/><button className="secondary fp-icon" aria-label={"Unplace "+nameOf(a.device_id)} onClick={()=>change({...plan,placements:plan.placements.filter(x=>x.device_id!==a.device_id)})}><Trash2 size={14}/></button></div>)}
    <div className="fp-place"><select className="secondary" aria-label="Device to place" value={pick} onChange={e=>setPick(e.target.value)}><option value="">Place a device on the plan…</option>{unplaced.map(d=><option key={d.id} value={d.id}>{d.name}</option>)}</select><button className="secondary" disabled={!pick} onClick={place}>Place</button></div>
   </div></details>}
 </section>;
}
