import { useEffect, useRef, useState } from "react";
import { Plus, Save, Trash2 } from "lucide-react";
import { apiFetch, apiJSON } from "../api";
import type { Device, FloorPlan as Plan, FloorRoom, Role } from "../types";

const STEP=0.5,MAX=500;
const snap=(v:number)=>Math.round(v/STEP)*STEP;
const clamp=(v:number,hi:number)=>Math.min(Math.max(v,0),hi);

// Rooms and AP positions in metres. Drawing only: nothing here touches a router.
export default function FloorPlan({devices,role,onChanged}:{devices:Device[];role:Role;onChanged:()=>void}){
 const canEdit=role!=="readonly";
 const [plan,setPlan]=useState<Plan>({rooms:[],placements:[]}),[dirty,setDirty]=useState(false),[saving,setSaving]=useState(false),[message,setMessage]=useState<{text:string;tone:"notice"|"error"}|null>(null);
 const [drag,setDrag]=useState<{kind:"room"|"ap";key:string;dx:number;dy:number}|null>(null),[pick,setPick]=useState("");
 const svg=useRef<SVGSVGElement>(null);
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
 const unplaced=devices.filter(d=>!plan.placements.some(a=>a.device_id===d.id));
 return <div className="audit-section"><div className="section-head"><div><h2>Floor plan</h2><p>Draw your rooms and drag each access point to where it sits. Metres; drawing only, nothing is changed on any router.</p></div><div className="tools">{canEdit&&<><button className="secondary" onClick={addRoom}><Plus size={15}/> Add room</button><button className="primary" disabled={!dirty||saving} onClick={()=>void save()}><Save size={15}/> {saving?"Saving…":"Save"}</button></>}</div></div>
 {message&&<div className={message.tone}>{message.text}<button onClick={()=>setMessage(null)}>Dismiss</button></div>}
 <div className="table-card"><svg ref={svg} viewBox={`0 0 ${extentX} ${extentY}`} style={{width:"100%",maxHeight:480,touchAction:"none",background:"var(--surface-2,#f6f7f9)"}} onPointerMove={move} onPointerUp={()=>setDrag(null)} role="img" aria-label="Floor plan">
  {plan.rooms.map(r=><g key={r.id}><rect x={r.x} y={r.y} width={r.w} height={r.h} fill="rgba(80,120,220,.08)" stroke="#5078dc" strokeWidth={0.06} onPointerDown={e=>start(e,"room",r.id,r.x,r.y)} style={{cursor:canEdit?"move":"default"}}/><text x={r.x+0.2} y={r.y+0.6} fontSize={0.5} fill="#345" pointerEvents="none">{r.name}</text></g>)}
  {plan.placements.map(a=><g key={a.device_id} onPointerDown={e=>start(e,"ap",a.device_id,a.x,a.y)} style={{cursor:canEdit?"grab":"default"}}><circle cx={a.x} cy={a.y} r={0.35} fill="#1f6feb" stroke="#fff" strokeWidth={0.08}/><text x={a.x+0.5} y={a.y+0.15} fontSize={0.45} fill="#123" pointerEvents="none">{nameOf(a.device_id)}</text></g>)}
 </svg></div>
 {canEdit&&<div className="table-card"><div className="table-scroll"><table><thead><tr><th>Room</th><th>Width (m)</th><th>Height (m)</th><th/></tr></thead><tbody>
  {plan.rooms.map(r=><tr key={r.id}><td><input maxLength={60} value={r.name} onChange={e=>patchRoom(r.id,{name:e.target.value})}/></td><td><input type="number" min={0.5} max={100} step={STEP} value={r.w} onChange={e=>patchRoom(r.id,{w:Math.max(0.5,Number(e.target.value)||0.5)})}/></td><td><input type="number" min={0.5} max={100} step={STEP} value={r.h} onChange={e=>patchRoom(r.id,{h:Math.max(0.5,Number(e.target.value)||0.5)})}/></td><td><button className="secondary" aria-label={"Remove "+r.name} onClick={()=>change({...plan,rooms:plan.rooms.filter(x=>x.id!==r.id)})}><Trash2 size={15}/></button></td></tr>)}
  {plan.placements.map(a=><tr key={a.device_id}><td colSpan={3}>Access point: <b>{nameOf(a.device_id)}</b></td><td><button className="secondary" aria-label={"Unplace "+nameOf(a.device_id)} onClick={()=>change({...plan,placements:plan.placements.filter(x=>x.device_id!==a.device_id)})}><Trash2 size={15}/></button></td></tr>)}
  <tr><td colSpan={3}><select className="secondary" value={pick} onChange={e=>setPick(e.target.value)}><option value="">Place a device…</option>{unplaced.map(d=><option key={d.id} value={d.id}>{d.name}</option>)}</select></td><td><button className="secondary" disabled={!pick} onClick={place}>Place</button></td></tr>
 </tbody></table></div></div>}</div>;
}
