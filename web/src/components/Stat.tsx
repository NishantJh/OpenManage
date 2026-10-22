import type { ReactNode } from "react";

export default function Stat({title,value,note,tone,icon}:{title:string;value:string;note:string;tone:string;icon:ReactNode}){return <div className="stat"><div className="stat-head"><span>{title}</span><span className={"stat-icon "+tone}>{icon}</span></div><strong className="stat-value">{value}</strong><small>{note}</small></div>}
