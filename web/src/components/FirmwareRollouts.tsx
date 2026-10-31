import { useEffect, useMemo, useState } from "react";
import { apiJSON } from "../api";
import type { Device, FirmwareHardwareQualification, FirmwarePlan, FirmwareRelease, FirmwareRollout, FirmwareRolloutQualificationStatus, Role } from "../types";
import UpgradeJob from "./UpgradeJob";

const requiredChecks=[
  ["image_hash_verified","Image hash verified"],
  ["pre_flash_backup_verified","Pre-flash backup verified"],
  ["sysupgrade_test_passed","Sysupgrade test passed"],
  ["post_reboot_reachable","Device reachable after reboot"],
  ["post_reboot_inventory_verified","Post-reboot inventory verified"],
  ["management_access_verified","Management access verified"],
] as const;
const emptyPlan:FirmwarePlan={version:"",version_code:"",kernel:"",target:"",profile:"",profile_title:"",package_manager:"opkg",packages:[],kept:[],renamed:[],dropped:[],unavailable:[],removed_defaults:[],blockers:[],warnings:[]};
const activeStatuses=["awaiting_canary","canary_running","awaiting_batch","batch_running"];
const stableRelease=(r:FirmwareRelease)=>!r.snapshot&&!r.release_candidate;
const defaultRelease=(releases:FirmwareRelease[])=>releases.find(stableRelease)?.version??"";
const short=(value:string)=>value.length>18?value.slice(0,10)+"…"+value.slice(-6):value;

type QualificationDraft={
  board_name:string;model:string;source_release:string;target_release:string;tested_on:string;
  physical_hardware:boolean;upgrade_checks:string[];upgrade_outcome:string;recovery_method:string;
  recovery_outcome:string;evidence_reference:string;notes:string;
};

export default function FirmwareRollouts({devices,role,onChanged}:{devices:Device[];role:Role;onChanged:()=>void}){
  const canAdmin=role==="admin",canRead=role==="admin"||role==="operator";
  const enrolled=useMemo(()=>devices.filter(d=>d.status!=="unknown"&&d.has_key),[devices]);
  const [rollouts,setRollouts]=useState<FirmwareRollout[]>([]);
  const [selectedID,setSelectedID]=useState<number|null>(null);
  const [detail,setDetail]=useState<FirmwareRollout|null>(null);
  const [qualification,setQualification]=useState<FirmwareRolloutQualificationStatus|null>(null);
  const [releases,setReleases]=useState<FirmwareRelease[]>([]);
  const [loading,setLoading]=useState(false),[busy,setBusy]=useState(false);
  const [error,setError]=useState(""),[notice,setNotice]=useState("");
  const [showCreate,setShowCreate]=useState(false);
  const [version,setVersion]=useState(""),[canaryID,setCanaryID]=useState("");
  const [assignments,setAssignments]=useState<Record<string,number>>({});
  const [maxConcurrent,setMaxConcurrent]=useState("2");
  const [pauseReason,setPauseReason]=useState("Paused by administrator");
  const [gapIndex,setGapIndex]=useState(0);
  const [qualificationDraft,setQualificationDraft]=useState<QualificationDraft>({
    board_name:"",model:"",source_release:"",target_release:"",tested_on:new Date().toISOString().slice(0,10),
    physical_hardware:false,upgrade_checks:[],upgrade_outcome:"passed",recovery_method:"failsafe",
    recovery_outcome:"passed",evidence_reference:"",notes:"",
  });

  async function loadRollouts(){
    try{
      const data=await apiJSON<FirmwareRollout[]>("/api/firmware/rollouts","GET",undefined,"Could not load firmware rollouts");
      setRollouts(data);
      setSelectedID(current=>current??data[0]?.id??null);
    }catch(e){setError(e instanceof Error?e.message:"Could not load firmware rollouts")}
  }
  async function loadDetail(id:number){
    try{
      const [campaign,qualificationStatus]=await Promise.all([
        apiJSON<FirmwareRollout>("/api/firmware/rollouts/"+id,"GET",undefined,"Could not load the rollout"),
        apiJSON<FirmwareRolloutQualificationStatus>("/api/firmware/rollouts/"+id+"/qualification-status","GET",undefined,"Could not load hardware qualification status"),
      ]);
      setDetail(campaign);setQualification(qualificationStatus);
    }catch(e){setError(e instanceof Error?e.message:"Could not load the selected rollout")}
  }
  useEffect(()=>{
    if(!canRead)return;
    setLoading(true);
    void Promise.all([
      loadRollouts(),
      apiJSON<{releases:FirmwareRelease[]}>("/api/firmware/releases","GET",undefined,"Could not load releases").then(d=>{setReleases(d.releases);setVersion(v=>v||defaultRelease(d.releases))}).catch(e=>setError(e instanceof Error?e.message:"Could not load releases")),
    ]).finally(()=>setLoading(false));
  },[canRead]);
  useEffect(()=>{if(selectedID!==null){setDetail(null);setQualification(null);void loadDetail(selectedID)}else{setDetail(null);setQualification(null)}},[selectedID]);
  useEffect(()=>{
    if(!detail||!activeStatuses.includes(detail.status))return;
    const timer=window.setInterval(()=>{void loadDetail(detail.id);void loadRollouts()},5000);
    return ()=>window.clearInterval(timer);
  },[detail?.id,detail?.status]);
  useEffect(()=>{
    const gap=qualification?.missing[gapIndex];
    if(!gap)return;
    setQualificationDraft(d=>({...d,board_name:gap.board_name,model:gap.model,source_release:gap.source_release,target_release:gap.target_release,physical_hardware:false,upgrade_checks:[],upgrade_outcome:"passed",recovery_method:"failsafe",recovery_outcome:"passed",evidence_reference:"",notes:""}));
  },[qualification?.missing,gapIndex]);

  function updateDraft<K extends keyof QualificationDraft>(key:K,value:QualificationDraft[K]){setQualificationDraft(d=>({...d,[key]:value}))}
  async function refresh(id=selectedID){
    await loadRollouts();
    if(id!==null)await loadDetail(id);
    onChanged();
  }
  async function createDraft(){
    if(!canaryID){setError("Select a canary device.");return}
    const selected=Object.entries(assignments).filter(([id])=>id!==canaryID);
    const highest=Math.max(0,...selected.map(([,batch])=>batch));
    const batches:string[][]=[];
    for(let batch=1;batch<=highest;batch++){
      const members=selected.filter(([,assigned])=>assigned===batch).map(([id])=>id);
      if(!members.length){setError("Batch numbers must be consecutive with no empty batches.");return}
      batches.push(members);
    }
    const body={version,canary_device_id:canaryID,batches,max_concurrent_flashes:Number(maxConcurrent)};
    setBusy(true);setError("");setNotice("");
    try{
      const created=await apiJSON<FirmwareRollout>("/api/firmware/rollouts","POST",body,"Could not create the rollout draft");
      setSelectedID(created.id);setDetail(created);setShowCreate(false);setAssignments({});
      try{
        await apiJSON<FirmwareRollout>("/api/firmware/rollouts/"+created.id+"/preview","POST",undefined,"Could not preview the rollout");
        await loadDetail(created.id);setNotice("Draft created and previewed. Review the immutable plans and physical qualification gate before starting.");
      }catch(e){setError(e instanceof Error?e.message:"Could not preview the rollout");await loadDetail(created.id)}
      await loadRollouts();onChanged();
    }catch(e){setError(e instanceof Error?e.message:"Could not create the rollout draft")}
    finally{setBusy(false)}
  }
  async function action(path:string,body?:unknown,success?:string){
    if(!detail)return;
    setBusy(true);setError("");setNotice("");
    try{
      await apiJSON("/api/firmware/rollouts/"+detail.id+path,"POST",body,"Rollout action failed");
      await refresh(detail.id);
      if(success)setNotice(success);
    }catch(e){setError(e instanceof Error?e.message:"Rollout action failed");await loadDetail(detail.id)}
    finally{setBusy(false)}
  }
  async function prepareDevice(deviceID:string){
    if(!detail)return;
    setBusy(true);setError("");setNotice("");
    try{
      await apiJSON("/api/firmware/rollouts/"+detail.id+"/devices/"+deviceID+"/prepare","POST",undefined,"Could not prepare the rollout device");
      await refresh(detail.id);setNotice("Device preparation started. The existing upgrade job will show progress and the individual flash confirmation.");
    }catch(e){setError(e instanceof Error?e.message:"Could not prepare the rollout device");await loadDetail(detail.id)}
    finally{setBusy(false)}
  }
  async function recordQualification(){
    if(!detail||!qualificationDraft.evidence_reference.trim()){setError("An evidence reference is required.");return}
    setBusy(true);setError("");setNotice("");
    try{
      await apiJSON<FirmwareHardwareQualification>("/api/firmware/hardware-qualifications","POST",qualificationDraft,"Could not record hardware qualification");
      await loadDetail(detail.id);onChanged();setNotice("Qualification record saved. The campaign gate has been recalculated.");
    }catch(e){setError(e instanceof Error?e.message:"Could not record hardware qualification")}
    finally{setBusy(false)}
  }
  const selected=detail;
  const canStart=Boolean(canAdmin&&selected?.status==="previewed"&&qualification?.qualified);
  const canAdvance=Boolean(canAdmin&&selected?.status==="awaiting_batch");
  const canPause=Boolean(canAdmin&&selected&&activeStatuses.includes(selected.status));
  const canCancel=Boolean(canAdmin&&selected&&!selected.devices.some(d=>d.upgrade_id)&&selected.status!=="complete"&&selected.status!=="cancelled");
  const canPreview=Boolean(canAdmin&&selected?.status==="draft");
  const currentBatchLabel=selected?.current_batch===0?"Canary":"Batch "+(selected?.current_batch??0);

  if(!canRead)return null;
  return <section className="firmware-rollouts">
    <div className="section-head"><div><h2>Fleet firmware rollouts</h2><p>Canary-first campaigns with physical qualification gates, immutable plans and bounded flash concurrency.</p></div><div className="tools">{canAdmin&&<button className="primary" onClick={()=>setShowCreate(v=>!v)}>{showCreate?"Close draft":"Create rollout draft"}</button>}<button className="secondary" disabled={loading||busy} onClick={()=>void refresh()}>Refresh</button></div></div>
    {error&&<div className="error backup-message">{error}</div>}
    {notice&&<div className="notice backup-message">{notice}</div>}
    {showCreate&&canAdmin&&<div className="table-card rollout-create">
      <div className="table-meta"><span>● &nbsp; New rollout draft</span><span>No device changes occur during draft creation or preview.</span></div>
      <div className="rollout-form-grid">
        <label>Target release<select className="secondary" value={version} onChange={e=>setVersion(e.target.value)}>{releases.filter(stableRelease).map(r=><option key={r.version} value={r.version}>{r.version} · {r.branch}</option>)}</select></label>
        <label>Canary device<select className="secondary" value={canaryID} onChange={e=>setCanaryID(e.target.value)}><option value="">Select canary</option>{enrolled.map(d=><option key={d.id} value={d.id}>{d.name} · {d.firmware||"release unknown"}</option>)}</select></label>
        <label>Maximum concurrent flashes<select className="secondary" value={maxConcurrent} onChange={e=>setMaxConcurrent(e.target.value)}><option value="1">1 device</option><option value="2">2 devices (maximum)</option></select></label>
      </div>
      <p className="firmware-read">Select additional devices and assign consecutive batches. The canary is always batch 0 and is the only device eligible for the first flash.</p>
      <div className="rollout-device-select">
        {enrolled.filter(d=>d.id!==canaryID).map(d=><div className="rollout-device-row" key={d.id}>
          <label><input type="checkbox" checked={assignments[d.id]!==undefined} onChange={e=>setAssignments(old=>{const next={...old};if(e.target.checked)next[d.id]=1;else delete next[d.id];return next})}/><span><b>{d.name}</b><small>{d.model||"Model unknown"} · {d.firmware||"Release unknown"} · {d.status}</small></span></label>
          {assignments[d.id]!==undefined&&<select className="secondary" aria-label={"Batch for "+d.name} value={assignments[d.id]} onChange={e=>setAssignments(old=>({...old,[d.id]:Number(e.target.value)}))}>{[1,2,3,4,5].map(n=><option key={n} value={n}>Batch {n}</option>)}</select>}
        </div>)}
        {!enrolled.length&&<p className="empty-line">Enroll devices before creating a rollout.</p>}
      </div>
      <div className="snapshot-controls"><button className="primary" disabled={busy||!version||!canaryID||!enrolled.length} onClick={()=>void createDraft()}>{busy?"Creating…":"Create and preview draft"}</button></div>
    </div>}
    <div className="rollout-layout">
      <div className="table-card rollout-list">
        <div className="table-meta"><span>● &nbsp; Campaigns &nbsp; <b>{rollouts.length}</b></span></div>
        {loading?<p className="empty-line">Loading campaigns…</p>:rollouts.length===0?<p className="empty-line">No fleet rollouts yet.</p>:rollouts.map(r=><button key={r.id} className={"rollout-list-item"+(selectedID===r.id?" selected":"")} onClick={()=>setSelectedID(r.id)}>
          <span><b>#{r.id} · OpenWrt {r.version}</b><small>{r.devices.length} devices · {r.created_at?new Date(r.created_at).toLocaleDateString():""}</small></span><strong className={"rollout-status status-"+r.status}>{r.status.replaceAll("_"," ")}</strong>
        </button>)}
      </div>
      <div className="rollout-detail">
        {!selected&&<div className="table-card"><p className="empty-line">Select a campaign or create a rollout draft.</p></div>}
        {selected&&<>
          <div className="table-card rollout-overview">
            <div className="table-meta"><span>● &nbsp; Campaign #{selected.id} · OpenWrt {selected.version}</span><strong className={"rollout-status status-"+selected.status}>{selected.status.replaceAll("_"," ")}</strong></div>
            <div className="rollout-facts"><div><small>ACTIVE STAGE</small><b>{currentBatchLabel}</b></div><div><small>FLASH CONCURRENCY</small><b>{selected.max_concurrent_flashes} maximum</b></div><div><small>DEVICES</small><b>{selected.devices.length}</b></div><div><small>CANARY</small><b>{selected.devices.find(d=>d.device_id===selected.canary_device_id)?.device_name??selected.canary_device_id}</b></div></div>
            {selected.pause_reason&&<div className="warning backup-message"><b>Paused:</b> {selected.pause_reason}<p>Do not retry a flash that may have been sent. Inspect the linked device job and use the documented physical recovery path if verification is inconclusive.</p></div>}
            <div className="snapshot-controls">
              {canPreview&&<button className="primary" disabled={busy} onClick={()=>void action("/preview",undefined,"Rollout preview saved.")}>Preview plans</button>}
              {canStart&&<button className="primary danger" disabled={busy} onClick={()=>void action("/start",undefined,"Canary gate opened. Prepare only the canary first.")}>Start campaign</button>}
              {canAdvance&&<button className="primary" disabled={busy} onClick={()=>void action("/advance",undefined,"Next batch released.")}>Release next batch</button>}
              {canPause&&<button className="secondary" disabled={busy} onClick={()=>void action("/pause",{reason:pauseReason})}>Pause campaign</button>}
              {canCancel&&<button className="secondary" disabled={busy} onClick={()=>void action("/cancel",undefined,"Campaign cancelled before device execution.")}>Cancel campaign</button>}
            </div>
            {canPause&&<label className="rollout-pause-reason">Pause reason<input value={pauseReason} maxLength={500} onChange={e=>setPauseReason(e.target.value)}/></label>}
            {selected.status==="previewed"&&qualification&&!qualification.qualified&&<p className="empty-line">Campaign cannot start until every hardware/release group has current physical qualification evidence.</p>}
          </div>
          {selected.status!=="draft"&&<div className="table-card rollout-qualification">
            <div className="table-meta"><span>● &nbsp; Physical hardware qualification</span><b>{qualification?.qualified?"QUALIFIED":"GATE NOT MET"}</b></div>
            {qualification&&<p>{qualification.matched.length} of {qualification.required_hardware} required hardware/release groups matched.</p>}
            {qualification?.matched.map(q=><div className="rollout-qualification-row" key={q.id}><b>{q.board_name} · {q.model}</b><span>{q.source_release} → {q.target_release}</span><small>Tested {q.tested_on} · recovery: {q.recovery_method} · evidence: {q.evidence_reference}</small></div>)}
            {qualification?.missing.map((gap,i)=><div className="rollout-qualification-row missing" key={gap.board_name+"|"+gap.model+"|"+gap.source_release}>
              <b>Missing qualification · {gap.board_name} · {gap.model}</b><span>{gap.source_release} → {gap.target_release}</span><small>{gap.device_ids.length} device(s): {gap.device_ids.map(id=>selected.devices.find(d=>d.device_id===id)?.device_name??short(id)).join(", ")}</small>
              {canAdmin&&<button className="secondary" onClick={()=>setGapIndex(i)}>Record evidence for this group</button>}
            </div>)}
            {canAdmin&&qualification?.missing[gapIndex]&&<div className="rollout-qualification-form">
              <h3>Record physical qualification</h3>
              <p>Record only tests actually completed on the matching physical hardware. The record is immutable and must include an evidence reference.</p>
              <div className="rollout-form-grid">
                <label>Board<input value={qualificationDraft.board_name} readOnly/></label>
                <label>Model<input value={qualificationDraft.model} readOnly/></label>
                <label>Source release<input value={qualificationDraft.source_release} readOnly/></label>
                <label>Target release<input value={qualificationDraft.target_release} readOnly/></label>
                <label>Tested on<input type="date" value={qualificationDraft.tested_on} onChange={e=>updateDraft("tested_on",e.target.value)}/></label>
                <label>Upgrade outcome<select className="secondary" value={qualificationDraft.upgrade_outcome} onChange={e=>updateDraft("upgrade_outcome",e.target.value)}><option value="passed">Passed</option><option value="failed">Failed</option></select></label>
                <label>Recovery method<select className="secondary" value={qualificationDraft.recovery_method} onChange={e=>updateDraft("recovery_method",e.target.value)}><option value="failsafe">Failsafe</option><option value="factory_reset">Factory reset</option><option value="serial_console">Serial console</option><option value="tftp">TFTP</option><option value="dual_image">Dual image</option><option value="not_exercised">Not exercised</option></select></label>
                <label>Recovery outcome<select className="secondary" value={qualificationDraft.recovery_outcome} onChange={e=>updateDraft("recovery_outcome",e.target.value)}><option value="passed">Passed</option><option value="failed">Failed</option></select></label>
              </div>
              <label className="check-row"><input type="checkbox" checked={qualificationDraft.physical_hardware} onChange={e=>updateDraft("physical_hardware",e.target.checked)}/> I tested this exact board/model on physical hardware.</label>
              <div className="rollout-checks"><b>Required upgrade checks</b>{requiredChecks.map(([key,label])=><label className="check-row" key={key}><input type="checkbox" checked={qualificationDraft.upgrade_checks.includes(key)} onChange={e=>updateDraft("upgrade_checks",e.target.checked?[...qualificationDraft.upgrade_checks,key]:qualificationDraft.upgrade_checks.filter(x=>x!==key))}/>{label}</label>)}</div>
              <label>Evidence reference<input value={qualificationDraft.evidence_reference} onChange={e=>updateDraft("evidence_reference",e.target.value)} placeholder="Lab run, ticket, report or test record"/></label>
              <label>Notes<textarea value={qualificationDraft.notes} onChange={e=>updateDraft("notes",e.target.value)} rows={2}/></label>
              <div className="snapshot-controls"><button className="primary" disabled={busy||!qualificationDraft.evidence_reference.trim()} onClick={()=>void recordQualification()}>Save qualification record</button></div>
            </div>}
          </div>}
          <div className="table-card rollout-members">
            <div className="table-meta"><span>● &nbsp; Device sequence</span><span>Current stage: {currentBatchLabel}</span></div>
            {selected.devices.map(member=><article className={"rollout-member"+(member.batch_number===selected.current_batch?" current":"")} key={member.device_id}>
              <div className="rollout-member-head">
                <div><b>{member.device_name}</b><small>{member.model} · {member.current_release} → {selected.version} · {member.board_name} / {member.target}</small></div>
                <div><span className="rollout-batch">{member.batch_number===0?"CANARY":"BATCH "+member.batch_number}</span><strong className={"rollout-status status-"+member.status}>{member.status.replaceAll("_"," ")}</strong></div>
              </div>
              {member.plan&&<div className="rollout-plan-summary"><span>Plan SHA-256 <code>{short(member.plan_digest??"")}</code></span><span>Inventory <code>{short(member.inventory_digest)}</code></span><span>Desired set revision <b>{member.desired_package_set_revision}</b></span><span>{member.plan.packages.length} package entries · {member.plan.warnings.length} warnings · {member.plan.blockers.length} blockers</span></div>}
              {member.plan?.blockers.map(item=><p className="error" key={item}>{item}</p>)}
              {member.plan?.warnings.map(item=><p className="warning" key={item}>{item}</p>)}
              {canAdmin&&member.status==="queued"&&member.batch_number===selected.current_batch&&((member.batch_number===0&&selected.status==="awaiting_canary")||(member.batch_number>0&&selected.status==="batch_running"))&&<div className="snapshot-controls"><button className="secondary" disabled={busy} onClick={()=>void prepareDevice(member.device_id)}>Prepare {member.device_name}</button></div>}
              {member.upgrade_id!==undefined&&member.upgrade_id!==null&&<UpgradeJob deviceId={member.device_id} deviceName={member.device_name} version={selected.version} role={role} plan={member.plan??emptyPlan} campaignMode upgradeId={member.upgrade_id} campaignActive={activeStatuses.includes(selected.status)}/>}
            </article>)}
          </div>
        </>}
      </div>
    </div>
  </section>;
}
