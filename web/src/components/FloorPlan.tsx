import { useEffect, useRef, useState } from "react";
import { Eye, EyeOff, Image as ImageIcon, Move, Plus, Radio, Save, Trash2, Upload, Wifi } from "lucide-react";
import { apiFetch, apiJSON, csrfToken } from "../api";
import type { Device, FloorPlan as Plan, FloorRoom, MonitoringSettings, Role, WifiClient, WifiClientEvent } from "../types";

// Plans are stored and drawn in metres; people see and type feet, snapped to whole feet.
const FT=0.3048,MAX=500,MIN_FT=2,MAX_FT=300,REFRESH_MS=60000;
const toFt=(m:number)=>Math.round(m/FT*10)/10;
const MIN_SIZE=MIN_FT*FT,MAX_SIZE=MAX_FT*FT;
const pollChoices:[number,string][]=[[60,"1 min"],[300,"5 min"],[600,"10 min"]];
const ago=(iso:string)=>{const m=Math.max(0,Math.round((Date.now()-new Date(iso).getTime())/60000));return m<1?"just now":m<60?m+" min ago":Math.round(m/60)+" h ago"};
const snap=(v:number)=>Math.round(v/FT)*FT;
const clamp=(v:number,hi:number)=>Math.min(Math.max(v,0),hi);
const inRoom=(r:FloorRoom,x:number,y:number)=>x>=r.x&&x<=r.x+r.w&&y>=r.y&&y<=r.y+r.h;
const bars=(dbm:number|null)=>dbm===null?0:dbm>-55?4:dbm>-65?3:dbm>-75?2:1;

// A feet field (the plan itself is in metres) that can be typed into freely (empty or
// half-typed values are kept as text) and only changes the plan once the text is a valid size.
function SizeInput({label,value,onCommit,minFt=MIN_FT}:{label:string;value:number;onCommit:(v:number)=>void;minFt?:number}){
 const [text,setText]=useState(String(toFt(value))),[focused,setFocused]=useState(false);
 useEffect(()=>{if(!focused)setText(String(toFt(value)))},[value,focused]);
 return <input aria-label={label} type="number" inputMode="decimal" min={minFt} max={MAX_FT} step={1} value={text} onFocus={()=>setFocused(true)} onBlur={()=>setFocused(false)} onChange={e=>{setText(e.target.value);const v=Number(e.target.value);if(e.target.value!==""&&v>=minFt&&v<=MAX_FT)onCommit(v*FT)}}/>;
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

const hash=(s:string)=>{let h=2166136261;for(const ch of s){h^=ch.charCodeAt(0);h=Math.imul(h,16777619)}return h>>>0};
const frac=(v:number)=>v-Math.floor(v);
// A client's dot goes in the room that holds the AP it is associated with (the nearest room if
// the AP sits outside every room). Each device gets its own stable spot inside that room, nudged
// apart so dots (minD plan units) do not overlap. Not a measured position.
export function placeDots(rooms:FloorRoom[],items:{id:string;ap:{x:number;y:number}}[],minD:number){
 const out=new Map<string,{x:number;y:number;room:string}>(),taken=new Map<string,{x:number;y:number}[]>();
 for(const it of [...items].sort((a,b)=>a.id<b.id?-1:1)){
  const room=rooms.find(r=>inRoom(r,it.ap.x,it.ap.y))??[...rooms].sort((a,b)=>Math.hypot(a.x+a.w/2-it.ap.x,a.y+a.h/2-it.ap.y)-Math.hypot(b.x+b.w/2-it.ap.x,b.y+b.h/2-it.ap.y))[0];
  if(!room){out.set(it.id,{x:it.ap.x+(frac(hash(it.id)/4294967296)-0.5)*2,y:it.ap.y+0.3+0.6*frac(hash(it.id+"y")/4294967296),room:""});continue}
  const used=taken.get(room.id)??[],m=Math.min(0.4,room.w/4,room.h/4),h1=hash(it.id)/4294967296,h2=hash(it.id+"y")/4294967296;
  let best={x:room.x+room.w/2,y:room.y+room.h/2},bestD=-1;
  for(let k=0;k<40;k++){
   const x=room.x+m+frac(h1+k*0.6180339887)*(room.w-2*m),y=room.y+m+frac(h2+k*0.7548776662)*(room.h-2*m);
   const d=used.length?Math.min(...used.map(u=>Math.hypot(u.x-x,u.y-y))):Infinity;
   if(d>bestD){best={x,y};bestD=d}
   if(d>=minD)break;
  }
  used.push(best);taken.set(room.id,used);out.set(it.id,{...best,room:room.name});
 }
 return out;
}

// Rooms and AP positions (kept in metres, shown in feet). Markers are drawn at a fixed pixel size so they stay small at any zoom, with the devices connected to each AP drawn on top.
// Drawing only: nothing here touches a router.
export default function FloorPlan({devices,role,onChanged}:{devices:Device[];role:Role;onChanged:()=>void}){
 const canEdit=role!=="readonly",isAdmin=role==="admin";
 const [plan,setPlan]=useState<Plan>({rooms:[],placements:[]}),[dirty,setDirty]=useState(false),[saving,setSaving]=useState(false),[message,setMessage]=useState<{text:string;tone:"notice"|"error"}|null>(null);
 const [adjust,setAdjust]=useState(false),[imgBusy,setImgBusy]=useState(false);
 const [drag,setDrag]=useState<{kind:"room"|"ap"|"size"|"image"|"imgsize";key:string;dx:number;dy:number;extent:{x:number;y:number}}|null>(null),[pick,setPick]=useState("");
 const [settings,setSettings]=useState<MonitoringSettings|null>(null),[clients,setClients]=useState<WifiClient[]>([]),[events,setEvents]=useState<WifiClientEvent[]>([]),[trackBusy,setTrackBusy]=useState(false);
 const svg=useRef<SVGSVGElement>(null),editor=useRef<HTMLDetailsElement>(null),[focusRoom,setFocusRoom]=useState(""),[hover,setHover]=useState(""),[hoverRoom,setHoverRoom]=useState(""),[width,setWidth]=useState(900);
 const tracking=Boolean(settings?.client_tracking_enabled);

 async function loadClients(){try{const [c,e]=await Promise.all([apiFetch("/api/wifi-clients"),apiFetch("/api/wifi-client-events?limit=20")]);if(c.ok)setClients(await c.json());if(e.ok)setEvents(await e.json())}catch{/* the map still works without client data */}}
 async function loadSettings(){try{const r=await apiFetch("/api/monitoring/settings");if(r.ok)setSettings(await r.json())}catch{/* shown as tracking off */}}
 useEffect(()=>{(async()=>{try{const r=await apiFetch("/api/floor-plan");if(!r.ok)throw Error();setPlan(await r.json())}catch{setMessage({tone:"error",text:"Could not load the floor plan."})}})();void loadSettings();void loadClients();const t=setInterval(()=>void loadClients(),REFRESH_MS);return()=>clearInterval(t)},[]);

 // Settings are read again just before saving so a monitoring change made elsewhere is not overwritten.
 async function saveTracking(enabled:boolean,interval:number){setTrackBusy(true);setMessage(null);try{const cur=await apiJSON<MonitoringSettings>("/api/monitoring/settings","GET",undefined,"Could not read monitoring settings");const data=await apiJSON<MonitoringSettings>("/api/monitoring/settings","PUT",{enabled:cur.enabled,interval_seconds:cur.interval_seconds,client_tracking_enabled:enabled,client_poll_interval_seconds:interval},"Could not save device tracking");setSettings(data);setMessage({tone:"notice",text:data.client_tracking_enabled?"Device tracking is on. Devices appear after the next poll.":"Device tracking is off. Data already collected stays until it expires or is cleared."})}catch(err){setMessage({tone:"error",text:err instanceof Error?err.message:"Could not save device tracking"})}finally{setTrackBusy(false)}}
 async function patchClient(c:WifiClient,change:{label?:string;hidden?:boolean}){setMessage(null);try{const r=await apiFetch("/api/wifi-clients/"+c.id,"PATCH",change);if(!r.ok)throw Error();setClients(prev=>prev.map(x=>x.id===c.id?{...x,...change}:x))}catch{setMessage({tone:"error",text:"Could not update the device."})}}
 async function clearClients(){if(!window.confirm("Delete every tracked device, its last location and its roaming history?"))return;try{const r=await apiFetch("/api/wifi-clients","DELETE");if(!r.ok)throw Error();setClients([]);setEvents([]);setMessage({tone:"notice",text:"Device data cleared."});onChanged()}catch{setMessage({tone:"error",text:"Could not clear device data."})}}

 useEffect(()=>{const el=svg.current;if(!el)return;const ro=new ResizeObserver(()=>setWidth(el.getBoundingClientRect().width||900));ro.observe(el);setWidth(el.getBoundingClientRect().width||900);return()=>ro.disconnect()},[]);
 useEffect(()=>{if(!focusRoom)return;const el=editor.current?.querySelector<HTMLInputElement>(`input[data-room="${focusRoom}"]`);el?.focus();el?.select();setFocusRoom("")},[focusRoom]);
 const nameOf=(id:string)=>devices.find(d=>d.id===id)?.name??"Unknown access point";
 const img=plan.image&&plan.image.px_w&&plan.image.px_h?{...plan.image,h:plan.image.width_m*plan.image.px_h/plan.image.px_w}:null;
 const fit={x:Math.max(6,...plan.rooms.map(r=>r.x+r.w+1),...plan.placements.map(a=>a.x+1),img?img.x+img.width_m+1:0),y:Math.max(4,...plan.rooms.map(r=>r.y+r.h+1),...plan.placements.map(a=>a.y+1),img?img.y+img.h+1:0)};
 // The canvas is held still while dragging so the pointer-to-plan mapping does not shift under the cursor.
 const {x:extentX,y:extentY}=drag?.extent??fit;
 const px=(extentX+PAD)/Math.max(width,200),P=(n:number)=>n*px; // P(n): n screen pixels in plan units
 const change=(next:Plan)=>{setPlan(next);setDirty(true)};
 function toPlan(e:{clientX:number;clientY:number}){const r=svg.current!.getBoundingClientRect();return {x:(e.clientX-r.left)/r.width*(extentX+PAD)-PAD,y:(e.clientY-r.top)/r.height*(extentY+PAD)-PAD}}
 function start(e:React.PointerEvent,kind:"room"|"ap"|"size"|"image"|"imgsize",key:string,ox:number,oy:number){if(!canEdit)return;const p=toPlan(e);(e.currentTarget as Element).setPointerCapture(e.pointerId);setDrag({kind,key,dx:p.x-ox,dy:p.y-oy,extent:fit})}
 function move(e:React.PointerEvent){if(!drag)return;const p=toPlan(e);
  if(drag.kind==="image"&&plan.image)change({...plan,image:{...plan.image,x:clamp(p.x-drag.dx,MAX),y:clamp(p.y-drag.dy,MAX)}});
  else if(drag.kind==="imgsize"&&plan.image)change({...plan,image:{...plan.image,width_m:Math.min(MAX_SIZE,Math.max(1,p.x-plan.image.x))}});
  else   if(drag.kind==="size")change({...plan,rooms:plan.rooms.map(r=>r.id===drag.key?{...r,w:Math.min(MAX_SIZE,Math.max(MIN_SIZE,snap(p.x-r.x))),h:Math.min(MAX_SIZE,Math.max(MIN_SIZE,snap(p.y-r.y)))}:r)});
  else if(drag.kind==="room")change({...plan,rooms:plan.rooms.map(r=>r.id===drag.key?{...r,x:clamp(snap(p.x-drag.dx),MAX-r.w),y:clamp(snap(p.y-drag.dy),MAX-r.h)}:r)});
  else{const pos=snapToWall(plan.rooms,clamp(snap(p.x-drag.dx),MAX),clamp(snap(p.y-drag.dy),MAX));change({...plan,placements:plan.placements.map(a=>a.device_id===drag.key?{...a,...pos}:a)})}}
 function renameRoom(id:string){if(editor.current)editor.current.open=true;setFocusRoom(id)}
 function addRoom(){const n=plan.rooms.length+1;change({...plan,rooms:[...plan.rooms,{id:"r"+Date.now().toString(36),name:"Room "+n,x:0,y:0,w:12*FT,h:10*FT}]})}
 function patchRoom(id:string,p:Partial<FloorRoom>){change({...plan,rooms:plan.rooms.map(r=>r.id===id?{...r,...p}:r)})}
 function place(){if(!pick)return;const r=plan.rooms[0],pos=r?{x:r.x+r.w/2,y:r.y}:{x:1,y:1};change({...plan,placements:[...plan.placements,{device_id:pick,...pos}]});setPick("")}
 // Uploading stores the picture on the server at once; only its facts are merged here so
 // room edits that are not saved yet are kept.
 async function uploadImage(file:File){setImgBusy(true);setMessage(null);try{const r=await fetch("/api/floor-plan/image",{method:"PUT",headers:{"X-CSRF-Token":csrfToken()},body:file});const data=await r.json();if(!r.ok)throw Error(data.error||"Could not upload the image");setPlan(p=>({...p,image:p.image&&data.image?{...data.image,x:p.image.x,y:p.image.y,width_m:p.image.width_m}:data.image}));setAdjust(true);setMessage({tone:"notice",text:"Plan image uploaded. Set its width in feet or drag its corner so it lines up, then draw the rooms over it."})}catch(err){setMessage({tone:"error",text:err instanceof Error?err.message:"Could not upload the image"})}finally{setImgBusy(false)}}
 async function removeImage(){if(!window.confirm("Remove the plan image? Rooms and access points stay."))return;setImgBusy(true);try{const r=await apiFetch("/api/floor-plan/image","DELETE");if(!r.ok)throw Error();setPlan(p=>({...p,image:undefined}));setAdjust(false)}catch{setMessage({tone:"error",text:"Could not remove the plan image."})}finally{setImgBusy(false)}}
 async function save(){setSaving(true);setMessage(null);try{const saved=await apiJSON<Plan>("/api/floor-plan","PUT",{rooms:plan.rooms,placements:plan.placements,image:plan.image?{x:plan.image.x,y:plan.image.y,width_m:plan.image.width_m}:undefined},"Could not save the floor plan");setPlan(saved);setDirty(false);setMessage({tone:"notice",text:"Floor plan saved."});onChanged()}catch(err){setMessage({tone:"error",text:err instanceof Error?err.message:"Could not save the floor plan"})}finally{setSaving(false)}}

 const apOf=(id:string)=>plan.placements.find(a=>a.device_id===id);
 const onMap=clients.filter(c=>!c.hidden&&apOf(c.device_id));
 const placed=placeDots(plan.rooms,onMap.map(c=>({id:c.id,ap:apOf(c.device_id)!})),P(16)),dots=onMap.map(c=>({c,...placed.get(c.id)!}));
 const unplaced=devices.filter(d=>!plan.placements.some(a=>a.device_id===d.id));
 const connectedCount=clients.filter(c=>c.connected).length;
 const eventText=(e:WifiClientEvent)=>e.kind==="roam"?"roamed"+(e.from_device_id?" from "+nameOf(e.from_device_id):"")+" to "+(e.to_device_id?nameOf(e.to_device_id):"another AP"):e.kind==="connect"?"joined "+(e.to_device_id?nameOf(e.to_device_id):"the network"):"left "+(e.from_device_id?nameOf(e.from_device_id):"the network");

 return <section className="audit-section fp">
  <div className="section-head"><div><h2>Floor plan</h2><p>Where your access points are and which devices are connected to them. Sizes in feet; drawing only, nothing is changed on any router.</p></div>
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
    <div className="fp-stage">
    <svg ref={svg} viewBox={`${-PAD} ${-PAD} ${extentX+PAD} ${extentY+PAD}`} className={"fp-svg"+(plan.image?" has-image":"")+(adjust&&img?" adjusting":"")} onPointerMove={move} onPointerUp={()=>setDrag(null)} role="img" aria-label="Floor plan">
     <defs>
      <pattern id="fp-grid" width="1" height="1" patternUnits="userSpaceOnUse"><path d="M1 0H0V1" fill="none" stroke="rgba(255,255,255,.035)" strokeWidth="0.02"/></pattern>
      <radialGradient id="fp-coverage"><stop offset="0" stopColor="#8d78f5" stopOpacity=".12"/><stop offset="1" stopColor="#8d78f5" stopOpacity="0"/></radialGradient>
      <radialGradient id="fp-glow-on"><stop offset="0" stopColor="#12a15e" stopOpacity=".6"/><stop offset=".5" stopColor="#12a15e" stopOpacity=".18"/><stop offset="1" stopColor="#12a15e" stopOpacity="0"/></radialGradient>
      <radialGradient id="fp-glow-off"><stop offset="0" stopColor="#e5484d" stopOpacity=".35"/><stop offset="1" stopColor="#e5484d" stopOpacity="0"/></radialGradient>
     </defs>
     <rect x={-PAD} y={-PAD} width={extentX+PAD} height={extentY+PAD} fill="url(#fp-grid)"/>
     {img&&<g>
      <image href={"/api/floor-plan/image?v="+(img.version??"")} x={img.x} y={img.y} width={img.width_m} height={img.h} preserveAspectRatio="none" className="fp-plan-image" onPointerDown={e=>adjust&&start(e,"image","img",img.x,img.y)}/>
      {canEdit&&adjust&&<><rect x={img.x} y={img.y} width={img.width_m} height={img.h} className="fp-image-frame" pointerEvents="none"/><rect x={img.x+img.width_m-P(5)} y={img.y+img.h-P(5)} width={P(10)} height={P(10)} rx={P(2)} className="fp-handle show img" onPointerDown={e=>start(e,"imgsize","img",0,0)}><title>Drag to scale the image</title></rect></>}</g>}
     {plan.rooms.map(r=><g key={r.id} onPointerEnter={()=>setHoverRoom(r.id)} onPointerLeave={()=>setHoverRoom("")}>
      <rect x={r.x} y={r.y} width={r.w} height={r.h} rx={P(3)} className="fp-room" onPointerDown={e=>start(e,"room",r.id,r.x,r.y)} style={{cursor:canEdit?"move":"default"}}/>
      <text x={r.x+P(14)} y={r.y+P(24)} className={canEdit?"fp-room-name edit":"fp-room-name"} style={{fontSize:P(11)}} onClick={()=>canEdit&&renameRoom(r.id)}>{canEdit&&<title>Click to rename</title>}{r.name.toUpperCase()}</text>
      <text x={r.x+P(14)} y={r.y+r.h-P(12)} className="fp-room-size" style={{fontSize:P(10)}}>{toFt(r.w)} × {toFt(r.h)} ft</text>
      {canEdit&&<rect x={r.x+r.w-P(5)} y={r.y+r.h-P(5)} width={P(10)} height={P(10)} rx={P(2)} className={hoverRoom===r.id||drag?.key===r.id?"fp-handle show":"fp-handle"} onPointerDown={e=>start(e,"size",r.id,0,0)}><title>Drag to resize</title></rect>}</g>)}
     {plan.placements.map(a=><g key={a.device_id} className="fp-ap-g" onPointerDown={e=>start(e,"ap",a.device_id,a.x,a.y)} style={{cursor:canEdit?"grab":"default"}}>
      <circle cx={a.x} cy={a.y} r={3} fill="url(#fp-coverage)" pointerEvents="none"/>
      <circle cx={a.x} cy={a.y} r={P(8)} className="fp-ap"/><circle cx={a.x} cy={a.y} r={P(3)} fill="#8d78f5" pointerEvents="none"/>
      <text x={a.x+P(14)} y={a.y+P(4)} className="fp-ap-name" style={{fontSize:P(12),strokeWidth:P(3)}} pointerEvents="none">{nameOf(a.device_id)}</text></g>)}
     {dots.map(({c,x,y})=><g key={c.id} onPointerEnter={()=>setHover(c.id)} onPointerLeave={()=>setHover("")}>
      <circle cx={x} cy={y} r={P(c.connected?9:6)} fill={c.connected?"url(#fp-glow-on)":"url(#fp-glow-off)"} pointerEvents="none"/>
      <circle cx={x} cy={y} r={P(c.connected?2.6:2.2)} className={c.connected?"fp-client on":"fp-client off"} pointerEvents="none"/>
      <circle cx={x} cy={y} r={P(9)} fill="transparent" className="fp-hit"/></g>)}
    </svg>
    {(()=>{const d=dots.find(v=>v.c.id===hover);if(!d)return null;const c=d.c,fx=(d.x+PAD)/(extentX+PAD),fy=(d.y+PAD)/(extentY+PAD);return <div className="fp-tip" role="status" style={{left:fx*100+"%",top:fy*100+"%",transform:`translate(${fx>0.6?"calc(-100% - 14px)":"14px"},${fy<0.2?"-15%":fy>0.8?"-85%":"-50%"})`}}><b>{c.label||c.name||"Unknown device"}</b><span>{nameOf(c.device_id)}{d.room?" · "+d.room:""}{c.connected&&c.signal_dbm!==null?" · "+c.signal_dbm+" dBm":""}</span><small>{c.mac_hint} · {c.connected?"connected":"last seen "+ago(c.last_seen)}</small></div>})()}
    </div>
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
  {canEdit&&<details ref={editor} className="fp-card fp-edit" open><summary><Radio size={15}/> Edit rooms and access points</summary>
   <div className="fp-edit-grid">
    <div className="fp-image-row">
     <span className="fp-image-title"><ImageIcon size={14}/> Plan image</span>
     <label className={"secondary fp-file"+(imgBusy?" disabled":"")}><Upload size={14}/> {plan.image?"Replace":"Upload"}<input type="file" accept="image/png,image/jpeg" disabled={imgBusy} aria-label="Upload plan image" onChange={e=>{const f=e.target.files?.[0];e.target.value="";if(f)void uploadImage(f)}}/></label>
     {plan.image&&<><button className={"secondary"+(adjust?" on":"")} aria-pressed={adjust} onClick={()=>setAdjust(!adjust)}><Move size={14}/> {adjust?"Done adjusting":"Move and scale"}</button>
      <span className="fp-image-width">Width (ft)<SizeInput label="Plan image width" minFt={3.3} value={plan.image.width_m} onCommit={w=>change({...plan,image:{...plan.image!,width_m:w}})}/></span>
      <button className="secondary fp-icon" aria-label="Remove plan image" disabled={imgBusy} onClick={()=>void removeImage()}><Trash2 size={14}/></button></>}
     <small>PNG or JPEG, up to 5 MB. Set the width of the whole picture in feet (or drag its corner) so it matches your real rooms.</small></div>
    {plan.rooms.length>0&&<div className="fp-edit-head"><span>Room</span><span>Width (ft)</span><span>Height (ft)</span><span/></div>}
    {plan.rooms.map(r=><div className="fp-edit-row" key={r.id}><input aria-label="Room name" data-room={r.id} maxLength={60} value={r.name} onChange={e=>patchRoom(r.id,{name:e.target.value})}/><SizeInput label={r.name+" width"} value={r.w} onCommit={w=>patchRoom(r.id,{w})}/><SizeInput label={r.name+" height"} value={r.h} onCommit={h=>patchRoom(r.id,{h})}/><button className="secondary fp-icon" aria-label={"Remove "+r.name} onClick={()=>change({...plan,rooms:plan.rooms.filter(x=>x.id!==r.id)})}><Trash2 size={14}/></button></div>)}
    {plan.placements.map(a=><div className="fp-edit-row fp-edit-ap" key={a.device_id}><span><i className="fp-dot ap"/> Access point <b>{nameOf(a.device_id)}</b></span><span/><span/><button className="secondary fp-icon" aria-label={"Unplace "+nameOf(a.device_id)} onClick={()=>change({...plan,placements:plan.placements.filter(x=>x.device_id!==a.device_id)})}><Trash2 size={14}/></button></div>)}
    <div className="fp-place"><select className="secondary" aria-label="Device to place" value={pick} onChange={e=>setPick(e.target.value)}><option value="">Place a device on the plan…</option>{unplaced.map(d=><option key={d.id} value={d.id}>{d.name}</option>)}</select><button className="secondary" disabled={!pick} onClick={place}>Place</button></div>
   </div></details>}
 </section>;
}
