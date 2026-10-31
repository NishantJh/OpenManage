import { useEffect, useMemo, useState } from "react";
import { RefreshCw, Router, ShieldCheck } from "lucide-react";
import { apiJSON } from "../api";
import type { Device, DeviceSettings, DeviceSettingsSchema, SettingsPageSchema } from "../types";
import { navigate } from "../nav";
import "../settings.css";

function deviceFromPath(){
 const match=window.location.pathname.match(/^\/settings\/([^/]+)\/?$/);
 return match?.[1]??"";
}

function fieldValue(settings:DeviceSettings|null,field:SettingsPageSchema["fields"][number]){
 const section=settings?.packages[field.config]?.sections.find(s=>s.name===field.section);
 const option=section?.options.find(o=>o.name===field.option);
 if(!option)return "Not set";
 if(option.secret||field.secret)return "Hidden · secret value";
 return option.value??"(empty)";
}

export default function DeviceSettings({devices}:{devices:Device[]}){
 const [deviceId,setDeviceId]=useState(deviceFromPath),[schema,setSchema]=useState<DeviceSettingsSchema|null>(null),[settings,setSettings]=useState<DeviceSettings|null>(null);
 const [pageKey,setPageKey]=useState("overview"),[loadingSchema,setLoadingSchema]=useState(true),[loadingSettings,setLoadingSettings]=useState(false),[error,setError]=useState("");
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
  setLoadingSettings(true);setSettings(null);setError("");
  apiJSON<DeviceSettings>("/api/devices/"+encodeURIComponent(deviceId)+"/settings","GET",undefined,"Could not read device settings",{signal:controller.signal})
   .then(data=>setSettings(data))
   .catch(e=>{if(!controller.signal.aborted)setError(e instanceof Error?e.message:"Could not read device settings")})
   .finally(()=>{if(!controller.signal.aborted)setLoadingSettings(false)});
  return()=>controller.abort();
 },[deviceId]);

 const pages=schema?.pages??[];
 const current=pages.find(p=>p.key===pageKey)??pages[0];
 const selectedDevice=devices.find(d=>d.id===deviceId);
 const packageNames=current?.key==="advanced"?Object.keys(settings?.packages??{}):current?.packages??[];

 function selectDevice(id:string){
  setDeviceId(id);
  navigate(id?"/settings/"+encodeURIComponent(id):"/settings");
 }

 return <div className="device-settings">
  <div className="section-head settings-toolbar"><div><h2>Device settings</h2><p>Read-only UCI view, organized like a router administration interface.</p></div><div className="tools"><label>Device<select aria-label="Settings device" className="secondary" value={deviceId} onChange={e=>selectDevice(e.target.value)}><option value="">Select an enrolled device</option>{enrolled.map(d=><option key={d.id} value={d.id}>{d.name} · {d.address}</option>)}</select></label><button className="secondary" onClick={()=>selectDevice(deviceId)} disabled={!deviceId||loadingSettings}><RefreshCw size={15}/> Refresh</button></div></div>
  {!enrolled.length&&<div className="notice">Enroll a device before viewing its settings. This view never changes device configuration.</div>}
  {error&&<div className="error">{error}<button className="secondary" onClick={()=>selectDevice(deviceId)}>Retry</button></div>}
  {deviceId&&!error&&selectedDevice&&<div className="settings-device-banner"><Router size={16}/><span><b>{selectedDevice.name}</b> · {selectedDevice.model||"OpenWrt device"} · {selectedDevice.address}</span><span className="read-only-tag"><ShieldCheck size={13}/> READ ONLY</span></div>}
  {deviceId&&!error&&loadingSettings&&<div className="health-loading"><RefreshCw size={16}/> Reading device settings…</div>}
  {!loadingSchema&&!schema&&<div className="notice">The settings schema is unavailable.</div>}
  {schema&&<div className="settings-layout">
   <nav className="settings-menu" aria-label="Device settings sections">
    {pages.map(p=><button type="button" key={p.key} className={p.key===(current?.key??pageKey)?"active":""} onClick={()=>setPageKey(p.key)}>{p.label}</button>)}
   </nav>
   <section className="settings-content" aria-label={current?.label??"Settings"}>
    <div className="settings-content-head"><div><div className="eyebrow">{current?.group.toUpperCase()}</div><h2>{current?.label}</h2><p>{current?.key==="software"?"Software and package management are available on the Firmware page.":current?.key==="advanced"?"Advanced configuration is displayed read-only; editing is not available yet.":"Values below are read live from the device. Secret values are hidden."}</p></div>{settings&&<small>Read {new Date(settings.captured_at).toLocaleString()}</small>}</div>
    {!deviceId?<div className="settings-empty"><Router size={24}/><b>Select an enrolled device</b><p>Choose a device above to read its current UCI settings.</p></div>:null}
    {deviceId&&settings&&current?.key==="software"&&<div className="settings-empty"><p>Open Firmware to inspect inventory and manage packages.</p><button className="secondary" onClick={()=>navigate("/firmware")}>Go to Firmware</button></div>}
    {deviceId&&settings&&current?.key!=="software"&&<>
     {current?.fields.length>0&&<div className="settings-fields">{current.fields.map(f=><div className="settings-field" key={f.key}><div><b>{f.label}</b><small>{f.help}</small></div><output className={f.secret?"secret-value":""}>{fieldValue(settings,f)}</output></div>)}</div>}
     {packageNames.map(name=>{
      const pkg=settings.packages[name];if(!pkg)return null;
      return <div className="table-card settings-package" key={name}><div className="table-meta"><span>{name} configuration</span><code title="Keyed HMAC token for stale-view detection; it does not reveal package values">{pkg.version.slice(0,12)}…</code></div>
       {pkg.sections.map(section=><div className="settings-section" key={section.name}><div className="settings-section-title"><b>{section.name}</b><small>{section.type}</small></div>
        {section.options.length===0?<p className="empty-line">No options in this section.</p>:<dl>{section.options.map(option=><div key={option.name}><dt>{option.name}</dt><dd className={option.secret?"secret-value":""}>{option.secret?"Hidden · secret value":option.value??"(empty)"}</dd></div>)}</dl>}
       </div>)}
      </div>;
     })}
     {packageNames.length===0&&current?.key!=="advanced"&&current?.key!=="overview"&&<div className="settings-empty"><p>No configuration packages are assigned to this section yet.</p></div>}
    </>}
   </section>
  </div>}
 </div>;
}
