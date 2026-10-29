import { navigate, usePage } from "../nav";
import type { PageKey } from "../nav";
import { Router, Activity, ShieldCheck, LayoutDashboard, Settings2, Database, MapPin, Cpu } from "lucide-react";

const roleLabel:Record<string,string>={admin:"Administrator",operator:"Operator",readonly:"Read-only"};

function Item({to,page,k,children}:{to:string;page:PageKey;k:PageKey;children:React.ReactNode}){
 return <a href={to} className={page===k?"active":""} aria-current={page===k?"page":undefined} onClick={e=>{if(e.metaKey||e.ctrlKey||e.shiftKey||e.button!==0)return;e.preventDefault();navigate(to)}}>{children}</a>;
}

export default function Sidebar({deviceCount,username,role}:{deviceCount:number;username:string;role:string}){
 const page=usePage();
 return <aside className="sidebar"><div className="brand"><div className="brand-icon"><Router size={21}/></div><div><b>openmanage</b><small>NETWORK CONTROL PLANE</small></div></div><div className="workspace"><span className="workspace-icon">H</span><div><b>Home network</b><small>Local environment</small></div></div><div className="nav-label">WORKSPACE</div><nav><Item to="/" k="overview" page={page}><LayoutDashboard size={17}/> Overview</Item><Item to="/devices" k="devices" page={page}><Router size={17}/> Devices <span className="count">{deviceCount}</span></Item><Item to="/floor-plan" k="floorplan" page={page}><MapPin size={17}/> Floor plan</Item><Item to="/configuration" k="configuration" page={page}><Settings2 size={17}/> Configuration</Item><Item to="/monitoring" k="monitoring" page={page}><Activity size={17}/> Monitoring</Item></nav><div className="nav-label">MANAGEMENT</div><nav><Item to="/access" k="access" page={page}><ShieldCheck size={17}/> Access & security</Item><Item to="/backups" k="backups" page={page}><Database size={17}/> Backups</Item><Item to="/firmware" k="firmware" page={page}><Cpu size={17}/> Firmware</Item></nav><div className="sidebar-bottom"><div className="side-status"><span className="green-dot"/><div><b>Control plane</b><small>Inventory preview</small></div></div><div className="profile"><span className="avatar">{username.slice(0,2).toUpperCase()}</span><div><b>{username}</b><small>{roleLabel[role]??role}</small></div></div></div></aside>;
}
