import { useEffect, useMemo, useState } from "react";
import type { FormEvent } from "react";
import { Eye, RefreshCw, Router, ShieldCheck } from "lucide-react";
import { apiJSON } from "../api";
import type { ConfigChangeBatchRecord, Device, DeviceSettings, DeviceSettingsSchema, SettingField, SettingsPageSchema } from "../types";
import { navigate } from "../nav";
import "../settings.css";

function deviceFromPath(){
 const match=window.location.pathname.match(/^\/settings\/([^/]+)\/?$/);
 return match?.[1]??"";
}

function fieldValue(settings:DeviceSettings|null,field:SettingField){
 const section=settings?.packages[field.config]?.sections.find(s=>s.name===field.section);
 if(!section)return "Not available on this device";
 const option=section?.options.find(o=>o.name===field.option);
 if(field.secret||option?.secret)return "Hidden · secret value";
 if(!option)return "Not set";
 return option.value??"(empty)";
}

function rawFieldValue(settings:DeviceSettings,field:SettingField):string|null{
 const section=settings.packages[field.config]?.sections.find(s=>s.name===field.section);
 if(!section)return null;
 const option=section.options.find(o=>o.name===field.option);
 if(option?.secret||field.secret)return null;
 return option?.value??"";
}

function SettingInput({field,value,disabled,onChange}:{field:SettingField;value:string;disabled:boolean;onChange:(value:string)=>void}){
 if(field.kind==="enum"){
   const options=field.options??[];
  return <select aria-label={field.label} className="secondary" value={value} disabled={disabled} onChange={e=>onChange(e.target.value)}>
   {!options.includes(value)&&<option value={value} disabled={!value}>{value||"Choose a value"} (current)</option>}
   {options.map(option=><option key={option} value={option}>{option}</option>)}
  </select>;
 }
 if(field.kind==="bool")return <select aria-label={field.label} className="secondary" value={value} disabled={disabled} onChange={e=>onChange(e.target.value)}><option value="0">Disabled</option><option value="1">Enabled</option></select>;
 return <input aria-label={field.label} type={field.kind==="int"?"number":"text"} inputMode={field.kind==="ipv4"?"decimal":undefined} min={field.min} max={field.max} step={field.kind==="int"?1:undefined} maxLength={1024} value={value} disabled={disabled} onChange={e=>onChange(e.target.value)}/>;
}

const batchStatusTone:Record<ConfigChangeBatchRecord["status"],string>={previewed:"unknown",applying:"degraded",applied:"online",rolled_back:"degraded",failed:"offline",expired:"unknown"};
const batchStatusText:Record<ConfigChangeBatchRecord["status"],string>={previewed:"PREVIEWED",applying:"APPLYING",applied:"APPLIED",rolled_back:"ROLLED BACK",failed:"FAILED",expired:"EXPIRED"};

export default function DeviceSettings({devices,role}:{devices:Device[];role:string}){
 const [deviceId,setDeviceId]=useState(deviceFromPath),[schema,setSchema]=useState<DeviceSettingsSchema|null>(null),[settings,setSettings]=useState<DeviceSettings|null>(null);
 const [pageKey,setPageKey]=useState("overview"),[loadingSchema,setLoadingSchema]=useState(true),[loadingSettings,setLoadingSettings]=useState(false),[error,setError]=useState("");
 const [drafts,setDrafts]=useState<Record<string,string>>({}),[preview,setPreview]=useState<ConfigChangeBatchRecord|null>(null),[history,setHistory]=useState<ConfigChangeBatchRecord[]>([]);
 const [unlockedFields,setUnlockedFields]=useState<Record<string,boolean>>({});
 const [ackDisruption,setAckDisruption]=useState(false),[busy,setBusy]=useState(false),[message,setMessage]=useState<{text:string;tone:"notice"|"error"}|null>(null);
 const enrolled=useMemo(()=>devices.filter(d=>d.has_key),[devices]);

 useEffect(()=>{
  const onPop=()=>setDeviceId(deviceFromPath());
  window.addEventListener("popstate",onPop);
  return()=>window.removeEventListener("popstate",onPop);
 },[]);

 useEffect(()=>{
  let alive=true;
  apiJSON<DeviceSettingsSchema>("/api/config-schema","GET",undefined,"Could not load the settings menu")
   .then(data=>{if(alive)setSchema(data)})
   .catch(e=>{if(alive)setError(e instanceof Error?e.message:"Could not load the settings menu")})
   .finally(()=>{if(alive)setLoadingSchema(false)});
  return()=>{alive=false};
 },[]);

 useEffect(()=>{
  if(!deviceId){setSettings(null);setError("");return}
  const controller=new AbortController();
  setLoadingSettings(true);setSettings(null);setError("");setDrafts({});setUnlockedFields({});setPreview(null);setHistory([]);setMessage(null);setAckDisruption(false);
  apiJSON<DeviceSettings>("/api/devices/"+encodeURIComponent(deviceId)+"/settings","GET",undefined,"Could not read device settings",{signal:controller.signal})
   .then(data=>setSettings(data))
   .catch(e=>{if(!controller.signal.aborted)setError(e instanceof Error?e.message:"Could not read device settings")})
   .finally(()=>{if(!controller.signal.aborted)setLoadingSettings(false)});
  return()=>controller.abort();
 },[deviceId]);

 useEffect(()=>{
  if(!deviceId||role!=="admin")return;
  let alive=true;
  apiJSON<ConfigChangeBatchRecord[]>("/api/devices/"+encodeURIComponent(deviceId)+"/config-change-batches","GET",undefined,"Could not load settings history")
   .then(data=>{if(alive)setHistory(data)})
   .catch(()=>{if(alive)setHistory([])});
  return()=>{alive=false};
 },[deviceId,role]);

 const pages=schema?.pages??[];
 const current=pages.find(p=>p.key===pageKey)??pages[0];
 const selectedDevice=devices.find(d=>d.id===deviceId);
 const packageNames=current?.key==="advanced"?Object.keys(settings?.packages??{}):current?.packages??[];
 const pageEditableFields=(current?.fields??[]).filter(f=>f.editable);
 const changedFields=pageEditableFields.filter(f=>{
  if(!settings)return false;
  const currentValue=rawFieldValue(settings,f);
  return currentValue!==null&&drafts[f.key]!==undefined&&drafts[f.key]!==currentValue;
 });
 const hasDisruptiveChange=changedFields.some(f=>f.risk==="disruptive");

 async function refreshSettings(){
  if(!deviceId)return;
  setLoadingSettings(true);setError("");
  try{
   const data=await apiJSON<DeviceSettings>("/api/devices/"+encodeURIComponent(deviceId)+"/settings","GET",undefined,"Could not refresh device settings");
   setSettings(data);setDrafts({});setUnlockedFields({});setPreview(null);setAckDisruption(false);
  }catch(e){setSettings(null);setError(e instanceof Error?e.message:"Could not refresh device settings")}
  finally{setLoadingSettings(false)}
 }

 async function loadHistory(){
  if(!deviceId||role!=="admin")return;
  try{setHistory(await apiJSON<ConfigChangeBatchRecord[]>("/api/devices/"+encodeURIComponent(deviceId)+"/config-change-batches","GET",undefined,"Could not load settings history"))}catch{/* History does not block reading or editing. */}
 }

 async function doPreview(e:FormEvent){
  e.preventDefault();if(!settings||!changedFields.length)return;
  setBusy(true);setMessage(null);setPreview(null);
  const baseVersions=Object.fromEntries(Object.entries(settings.packages).map(([name,pkg])=>[name,pkg.version]));
  try{
   const data=await apiJSON<ConfigChangeBatchRecord>("/api/devices/"+encodeURIComponent(deviceId)+"/config-change-batches","POST",{
    base_versions:baseVersions,changes:changedFields.map(f=>({setting:f.key,value:drafts[f.key],unlock_existing:f.unlock_required&&rawFieldValue(settings,f)!==""?Boolean(unlockedFields[f.key]):false})),acknowledge_disruption:ackDisruption,
   },"Could not preview the settings changes");
   setPreview(data);await loadHistory();
  }catch(e){setMessage({tone:"error",text:e instanceof Error?e.message:"Could not preview the settings changes"})}
  finally{setBusy(false)}
 }

 async function applyPreview(){
  if(!preview)return;
  setBusy(true);setMessage(null);
  try{
   const data=await apiJSON<ConfigChangeBatchRecord>("/api/devices/"+encodeURIComponent(deviceId)+"/config-change-batches/"+preview.id+"/apply","POST",undefined,"Could not apply the settings changes");
   setPreview(null);setMessage({tone:data.status==="applied"?"notice":"error",text:data.status==="applied"?"Settings were applied, verified over a fresh connection, and confirmed.":(data.status==="rolled_back"?"Rolled back: ":"Not applied: ")+data.detail+"."});
   await refreshSettings();
  }catch(e){setMessage({tone:"error",text:e instanceof Error?e.message:"Could not apply the settings changes"})}
  finally{setBusy(false);await loadHistory()}
 }

 function selectDevice(id:string){
  setDrafts({});setUnlockedFields({});setPreview(null);setMessage(null);setAckDisruption(false);
  setDeviceId(id);
  navigate(id?"/settings/"+encodeURIComponent(id):"/settings");
 }

 return <div className="device-settings">
  <div className="section-head settings-toolbar"><div><h2>Device settings</h2><p>Curated settings use a preview-and-rollback workflow; other UCI values stay read-only.</p></div><div className="tools"><label>Device<select aria-label="Settings device" className="secondary" value={deviceId} onChange={e=>selectDevice(e.target.value)}><option value="">Select an enrolled device</option>{enrolled.map(d=><option key={d.id} value={d.id}>{d.name} · {d.address}</option>)}</select></label><button className="secondary" onClick={()=>void refreshSettings()} disabled={!deviceId||loadingSettings||busy}><RefreshCw size={15}/> Refresh</button></div></div>
  {!enrolled.length&&<div className="notice">Enroll a device before viewing its settings.</div>}
  {error&&<div className="error">{error}<button className="secondary" onClick={()=>selectDevice(deviceId)}>Retry</button></div>}
  {deviceId&&!error&&selectedDevice&&<div className="settings-device-banner"><Router size={16}/><span><b>{selectedDevice.name}</b> · {selectedDevice.model||"OpenWrt device"} · {selectedDevice.address}</span><span className="read-only-tag"><ShieldCheck size={13}/> {role==="admin"?"ROLLBACK PROTECTED":"READ ONLY"}</span></div>}
  {deviceId&&!error&&loadingSettings&&<div className="health-loading"><RefreshCw size={16}/> Reading device settings…</div>}
  {!loadingSchema&&!schema&&<div className="notice">The settings schema is unavailable.</div>}
  {schema&&<div className="settings-layout">
   <nav className="settings-menu" aria-label="Device settings sections">
     {pages.map(p=><button type="button" key={p.key} className={p.key===(current?.key??pageKey)?"active":""} onClick={()=>{setPageKey(p.key);setPreview(null);setUnlockedFields({});setAckDisruption(false);setMessage(null)}}>{p.label}</button>)}
   </nav>
   <section className="settings-content" aria-label={current?.label??"Settings"}>
     <div className="settings-content-head"><div><div className="eyebrow">{current?.group.toUpperCase()}</div><h2>{current?.label}</h2><p>{current?.key==="software"?"Software and package management are available on the Firmware page.":current?.key==="advanced"?"Advanced configuration is displayed read-only; editing is not available yet.":"Values are read live. Secret values stay hidden; passphrase editing is not enabled yet."}</p></div>{settings&&<small>Read {new Date(settings.captured_at).toLocaleString()}</small>}</div>
    {!deviceId?<div className="settings-empty"><Router size={24}/><b>Select an enrolled device</b><p>Choose a device above to read its current UCI settings.</p></div>:null}
    {deviceId&&settings&&current?.key==="software"&&<div className="settings-empty"><p>Open Firmware to inspect inventory and manage packages.</p><button className="secondary" onClick={()=>navigate("/firmware")}>Go to Firmware</button></div>}
    {deviceId&&settings&&current?.key!=="software"&&<>
     {current?.fields.length>0&&<div className="settings-fields">{current.fields.map(f=>{
      const currentValue=rawFieldValue(settings,f),editable=role==="admin"&&f.editable&&currentValue!==null;
      const locked=f.unlock_required&&currentValue!==""&&!unlockedFields[f.key];
      return <div className="settings-field" key={f.key}><div><b>{f.label}</b><small>{f.help}</small></div>{editable?<div className="settings-editor-control"><small>Current: <output>{fieldValue(settings,f)}</output></small>{f.unlock_required&&currentValue!==""&&<label className="settings-field-unlock"><input type="checkbox" checked={Boolean(unlockedFields[f.key])} onChange={e=>{setUnlockedFields(s=>({...s,[f.key]:e.target.checked}));setPreview(null);setMessage(null);setAckDisruption(false)}}/> Unlock this existing SSID to edit it</label>}<SettingInput field={f} value={drafts[f.key]??currentValue} disabled={busy||locked} onChange={value=>{setDrafts(d=>({...d,[f.key]:value}));setPreview(null);setMessage(null);setAckDisruption(false)}}/></div>:<output className={f.secret?"secret-value":""}>{fieldValue(settings,f)}</output>}</div>;
     })}</div>}
     {role==="admin"&&pageEditableFields.length>0&&<form className="settings-batch-editor" onSubmit={doPreview}>
      <div><b>Edit curated settings</b><p>Only changed fields are sent. OpenManage checks this full-page settings version again before preview and apply. Secrets and management-access settings remain locked.</p></div>
      {hasDisruptiveChange&&<label className="settings-risk-ack"><input type="checkbox" checked={ackDisruption} onChange={e=>setAckDisruption(e.target.checked)}/> I understand these changes can disconnect wireless clients or interrupt network access while the router applies them.</label>}
      <button className="secondary" type="submit" disabled={busy||!changedFields.length||(hasDisruptiveChange&&!ackDisruption)}><Eye size={15}/> Preview {changedFields.length||""} change{changedFields.length===1?"":"s"}</button>
     </form>}
     {message&&<div className={message.tone}>{message.text}</div>}
     {preview&&<div className="change-preview settings-batch-preview"><div><b>Settings preview · {preview.changes.length} change{preview.changes.length===1?"":"s"}</b><div className="settings-batch-diff">{preview.changes.map(c=><div key={c.setting}><b>{c.label}</b><span className="config-value">{c.old_value||"(not set)"}</span> → <span className="config-value">{c.new_value||"(empty)"}</span></div>)}</div><small className="field-help">Expires {new Date(preview.expires_at).toLocaleTimeString()}. Applying arms a 60-second OpenWrt rollback and verifies over a fresh connection.</small></div><button className="primary" type="button" disabled={busy} onClick={()=>void applyPreview()}><ShieldCheck size={15}/> {busy?"Applying…":"Apply with rollback"}</button></div>}
     {packageNames.map(name=>{
      const pkg=settings.packages[name];if(!pkg)return null;
      return <div className="table-card settings-package" key={name}><div className="table-meta"><span>{name} configuration</span><code title="Keyed HMAC token for stale-view detection; it does not reveal package values">{pkg.version.slice(0,12)}…</code></div>
       {pkg.sections.map(section=><div className="settings-section" key={section.name}><div className="settings-section-title"><b>{section.name}</b><small>{section.type}</small></div>
        {section.options.length===0?<p className="empty-line">No options in this section.</p>:<dl>{section.options.map(option=><div key={option.name}><dt>{option.name}</dt><dd className={option.secret?"secret-value":""}>{option.secret?"Hidden · secret value":option.value??"(empty)"}</dd></div>)}</dl>}
       </div>)}
      </div>;
     })}
     {packageNames.length===0&&current?.key!=="advanced"&&current?.key!=="overview"&&<div className="settings-empty"><p>No configuration packages are assigned to this section yet.</p></div>}
     {role==="admin"&&history.length>0&&<div className="table-card settings-batch-history"><div className="table-meta"><span>Settings change history</span><span>Newest first · latest 50</span></div><div className="table-scroll"><table><thead><tr><th>CHANGES</th><th>RESULT</th><th>WHEN</th></tr></thead><tbody>{history.slice(0,10).map(change=><tr key={change.id}><td>{change.changes.map(c=><div className="settings-history-item" key={c.setting}><b>{c.label}</b><span className="config-value">{c.old_value||"(not set)"}</span> → <span className="config-value">{c.new_value||"(empty)"}</span></div>)}</td><td><span className={"status "+batchStatusTone[change.status]}><i/>{batchStatusText[change.status]}</span>{change.detail&&<small>{change.detail}</small>}</td><td className="last-seen">{new Date(change.finished_at??change.created_at).toLocaleString()}</td></tr>)}</tbody></table></div></div>}
    </>}
   </section>
  </div>}
 </div>;
}
