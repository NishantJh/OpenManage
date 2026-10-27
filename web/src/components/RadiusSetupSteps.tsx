import { AlertTriangle, Copy } from "lucide-react";
import { useEffect, useState } from "react";
import { apiFetch } from "../api";

const quote=(s:string)=>"'"+s.replace(/'/g,"'\\''")+"'";
// wideModal fits the commands on screen; long dialogs scroll inside the overlay.
export const wideModal={width:"min(680px,100%)",maxHeight:"calc(100vh - 36px)",overflowY:"auto"} as const;
const ipv4=/^\d{1,3}(\.\d{1,3}){3}$/;

// apCommands switch the AP's existing access-point networks (all of them, or only the
// named SSIDs) to WPA2-Enterprise checked by OpenManage. Nothing new is created.
export function apCommands(o:{server:string;port:string;secret:string;ssids:string[]}){
 const only=o.ssids.length?[`  case "$(uci -q get wireless.$s.ssid)" in ${o.ssids.map(quote).join("|")}) ;; *) continue ;; esac`]:[];
 return [
  `for s in $(uci show wireless | sed -n 's/^wireless\\.\\([^.=]*\\)=wifi-iface$/\\1/p'); do`,
  `  [ "$(uci -q get wireless.$s.mode)" = ap ] || continue`,
  ...only,
  `  uci set wireless.$s.encryption='wpa2'`,
  `  uci set wireless.$s.auth_server=${quote(o.server)}`,
  `  uci set wireless.$s.auth_port=${quote(o.port)}`,
  `  uci set wireless.$s.auth_secret=${quote(o.secret)}`,
  `  echo "RADIUS on: $(uci -q get wireless.$s.ssid)"`,
  "done",
  "uci commit wireless",
  "wifi reload",
 ].join("\n");
}
export const checkCommand="(opkg list-installed 2>/dev/null; apk list --installed 2>/dev/null) | grep wpad";
export const backupCommand="cp /etc/config/wireless /root/wireless.before-radius";
export const undoCommands="cp /root/wireless.before-radius /etc/config/wireless\nwifi reload";

function Block({label,text}:{label:string;text:string}){
 const [copied,setCopied]=useState(false);
 return <div style={{position:"relative",marginBottom:12}}><pre aria-label={label} style={{margin:0,padding:"10px 12px",paddingRight:70,background:"#0f1219",border:"1px solid #303543",borderRadius:7,fontSize:10.5,lineHeight:1.6,color:"#d7dae4",overflowX:"auto",whiteSpace:"pre"}}>{text}</pre><button type="button" className="secondary" style={{position:"absolute",top:6,right:6,padding:"5px 8px"}} onClick={()=>{void navigator.clipboard.writeText(text).then(()=>setCopied(true),()=>setCopied(false))}}><Copy size={13}/> {copied?"Copied":"Copy"}</button></div>;
}

// RadiusSetupSteps shows a new RADIUS shared secret with every step needed to use it on the AP.
export default function RadiusSetupSteps({deviceName,deviceAddress,secret}:{deviceName:string;deviceAddress:string;secret:string}){
 const host=typeof location!=="undefined"?location.hostname:"";
 const [server,setServer]=useState(ipv4.test(host)?host:""),[port,setPort]=useState("1812"),[networks,setNetworks]=useState("");
 useEffect(()=>{apiFetch("/api/radius").then(r=>r.ok?r.json():null).then(d=>{const p=String(d?.listen_addr??"").split(":").pop();if(p&&/^\d+$/.test(p))setPort(p)}).catch(()=>{})},[]);
 const ssids=networks.split(",").map(s=>s.trim()).filter(Boolean);
 const small={fontSize:10,lineHeight:1.6,color:"#888e9e",fontWeight:400} as const;
 return <div>
  <label>RADIUS shared secret<input readOnly value={secret} onFocus={e=>e.currentTarget.select()} aria-label="RADIUS shared secret"/><small className="field-help">Shown only now. The commands below already contain it. If it is lost, rotate it on the Configuration page.</small></label>
  <div className="form-row"><label>OpenManage LAN address<input value={server} onChange={e=>setServer(e.target.value.trim())} placeholder="e.g. 192.168.1.10" aria-label="OpenManage LAN address"/></label><label>RADIUS port<input value={port} onChange={e=>setPort(e.target.value.trim())} aria-label="RADIUS port"/></label></div>
  <label>Wi-Fi networks to change<input value={networks} onChange={e=>setNetworks(e.target.value)} placeholder="All Wi-Fi networks on this device" aria-label="Wi-Fi networks to change"/><small className="field-help">Leave empty for every Wi-Fi network the device broadcasts, or list network names separated by commas. The address must be the IP address of the machine running OpenManage on your LAN.</small></label>
  <div className="warning" style={{marginBottom:15}}><AlertTriangle size={15}/><span>Your existing networks keep their names and radios but switch to per-person sign-in. Phones and laptops then join with a Wi-Fi user. Devices that only know the old Wi-Fi password, such as printers or smart plugs, will drop off those networks.</span></div>
  <ol style={{paddingLeft:18,margin:"0 0 6px",color:"#bfc3cf",fontSize:11,lineHeight:1.6}}>
   <li>Sign in to {deviceName} over SSH from a computer on your network:<Block label="SSH command" text={"ssh root@"+deviceAddress}/></li>
   <li>Check that it can do WPA2-Enterprise. If this shows <code>wpad-basic-…</code>, install <code>wpad-mbedtls</code> first (System, Software in LuCI):<Block label="Check command" text={checkCommand}/></li>
   <li>Save its current Wi-Fi settings, so you can go back:<Block label="Backup command" text={backupCommand}/></li>
   <li>Turn on RADIUS on {ssids.length?"the listed networks":"its existing Wi-Fi networks"}. Paste all lines at once; it prints each network it changed:<Block label="AP setup commands" text={apCommands({server:server||"<OpenManage LAN address>",port,secret,ssids})}/></li>
   <li>On the iPhone, install the setup profile from Configuration, Server certificate, then join the network with a Wi-Fi user.</li>
  </ol>
  <p style={small}>To undo, put back the saved settings:</p><Block label="Undo commands" text={undoCommands}/>
 </div>;
}
