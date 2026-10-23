import { useEffect, useState } from "react";

export const pages=[
 {path:"/",key:"overview",label:"Overview"},
 {path:"/devices",key:"devices",label:"Devices"},
 {path:"/floor-plan",key:"floorplan",label:"Floor plan"},
 {path:"/configuration",key:"configuration",label:"Configuration"},
 {path:"/monitoring",key:"monitoring",label:"Monitoring"},
 {path:"/access",key:"access",label:"Access & security"},
 {path:"/backups",key:"backups",label:"Backups"},
] as const;
export type PageKey=typeof pages[number]["key"];

function currentPage():PageKey{
 const p=window.location.pathname.replace(/\/+$/,"")||"/";
 return pages.find(x=>x.path===p)?.key??"overview";
}

export function navigate(path:string){
 if(window.location.pathname!==path)window.history.pushState(null,"",path);
 window.dispatchEvent(new PopStateEvent("popstate"));
 window.scrollTo(0,0);
}

export function usePage():PageKey{
 const [page,setPage]=useState<PageKey>(currentPage);
 useEffect(()=>{const on=()=>setPage(currentPage());window.addEventListener("popstate",on);return()=>window.removeEventListener("popstate",on)},[]);
 return page;
}
