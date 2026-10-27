import { Copy } from "lucide-react";
import { useEffect, useState } from "react";
import { apiFetch } from "../api";

// The wireless section the commands create, so undo removes only what was added.
const section="omradius";
const quote=(s:string)=>"'"+s.replace(/'/g,"'\\''")+"'";
// wideModal fits the commands on screen; long dialogs scroll inside the overlay.
export const wideModal={width:"min(680px,100%)",maxHeight:"calc(100vh - 36px)",overflowY:"auto"} as const;
const ipv4=/^\d{1,3}(\.\d{1,3}){3}$/;

// apCommands are the uci lines that add a WPA2-Enterprise network pointing at OpenManage.
export function apCommands(o:{server:string;port:string;secret:string;ssid:string;radio:string}){
 return [
  `uci set wireless.${section}=wifi-iface`,
  `uci set wireless.${section}.device=${quote(o.radio)}`,
  `uci set wireless.${section}.mode='ap'`,
  `uci set wireless.${section}.network='lan'`,
  `uci set wireless.${section}.ssid=${quote(o.ssid)}`,
  `uci set wireless.${section}.encryption='wpa2'`,
  `uci set wireless.${section}.auth_server=${quote(o.server)}`,
  `uci set wireless.${section}.auth_port=${quote(o.port)}`,
  `uci set wireless.${section}.auth_secret=${quote(o.secret)}`,
  "uci commit wireless",
  "wifi reload",
 ].join("\n");
}
export const backupCommand="cp /etc/config/wireless /root/wireless.before-radius";
export const undoCommands=`uci delete wireless.${section}\nuci commit wireless\nwifi reload`;

function Block({label,text}:{label:string;text:string}){
 const [copied,setCopied]=useState(false);
 return <div style={{position:"relative",marginBottom:12}}><pre aria-label={label} style={{margin:0,padding:"10px 12px",paddingRight:70,background:"#0f1219",border:"1px solid #303543",borderRadius:7,fontSize:10.5,lineHeight:1.6,color:"#d7dae4",overflowX:"auto",whiteSpace:"pre"}}>{text}</pre><button type="button" className="secondary" style={{position:"absolute",top:6,right:6,padding:"5px 8px"}} onClick={()=>{void navigator.clipboard.writeText(text).then(()=>setCopied(true),()=>setCopied(false))}}><Copy size={13}/> {copied?"Copied":"Copy"}</button></div>;
}

// RadiusSetupSteps shows a new RADIUS shared secret with every step needed to use it on the AP.
export default function RadiusSetupSteps({deviceName,deviceAddress,secret}:{deviceName:string;deviceAddress:string;secret:string}){
 const host=typeof location!=="undefined"?location.hostname:"";
 const [server,setServer]=useState(ipv4.test(host)?host:""),[port,setPort]=useState("1812"),[ssid,setSsid]=useState("Home-Secure"),[radio,setRadio]=useState("radio0");
 useEffect(()=>{apiFetch("/api/radius").then(r=>r.ok?r.json():null).then(d=>{const p=String(d?.listen_addr??"").split(":").pop();if(p&&/^\d+$/.test(p))setPort(p)}).catch(()=>{})},[]);
 const small={fontSize:10,lineHeight:1.6,color:"#888e9e",fontWeight:400} as const;
 return <div>
  <label>RADIUS shared secret<input readOnly value={secret} onFocus={e=>e.currentTarget.select()} aria-label="RADIUS shared secret"/><small className="field-help">Shown only now. The commands below already contain it. If it is lost, rotate it on the Configuration page.</small></label>
  <div className="form-row"><label>OpenManage LAN address<input value={server} onChange={e=>setServer(e.target.value.trim())} placeholder="e.g. 192.168.1.10" aria-label="OpenManage LAN address"/></label><label>RADIUS port<input value={port} onChange={e=>setPort(e.target.value.trim())} aria-label="RADIUS port"/></label></div>
  <div className="form-row"><label>Wi-Fi network name<input value={ssid} maxLength={32} onChange={e=>setSsid(e.target.value)} aria-label="Wi-Fi network name"/></label><label>Radio<input value={radio} onChange={e=>setRadio(e.target.value.trim())} aria-label="Radio"/></label></div>
  <p style={small}>The address must be the IP address of the machine running OpenManage on your LAN. Radio: <code>radio0</code> is usually 2.4 GHz and <code>radio1</code> 5 GHz; <code>uci show wireless | grep =wifi-device</code> on the AP lists them.</p>
  <ol style={{paddingLeft:18,margin:"0 0 6px",color:"#bfc3cf",fontSize:11,lineHeight:1.6}}>
   <li>Sign in to {deviceName} over SSH from a computer on your network:<Block label="SSH command" text={"ssh root@"+deviceAddress}/></li>
   <li>Save its current Wi-Fi settings, so you can go back:<Block label="Backup command" text={backupCommand}/></li>
   <li>Add a new Wi-Fi network that checks sign-ins with OpenManage. Your existing networks are not changed. Paste all lines at once:<Block label="AP setup commands" text={apCommands({server:server||"<OpenManage LAN address>",port,secret,ssid,radio})}/></li>
   <li>On the iPhone, install the setup profile from Configuration, Server certificate, then join <b>{ssid||"the network"}</b> with a Wi-Fi user.</li>
  </ol>
  <p style={small}>To undo, run this on the AP:</p><Block label="Undo commands" text={undoCommands}/>
  <p style={small}>Or put back everything as it was: <code>cp /root/wireless.before-radius /etc/config/wireless; wifi reload</code></p>
 </div>;
}
