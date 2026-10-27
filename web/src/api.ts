// Every API call goes through here so state-changing requests always carry the CSRF token.
export function csrfToken(){return document.cookie.split("; ").find(c=>c.startsWith("om_csrf="))?.slice("om_csrf=".length)??""}

type Method="GET"|"POST"|"PUT"|"PATCH"|"DELETE";

// apiFetch sends a request with the CSRF header on every non-GET call and a JSON body when given one.
export function apiFetch(path:string,method:Method="GET",body?:unknown,{csrf=true,signal}:{csrf?:boolean;signal?:AbortSignal}={}){
 const headers:Record<string,string>={};
 if(body!==undefined)headers["Content-Type"]="application/json";
 if(method!=="GET"&&csrf)headers["X-CSRF-Token"]=csrfToken();
 return fetch(path,{method,headers,body:body===undefined?undefined:JSON.stringify(body),signal});
}

// apiJSON returns the parsed body, or throws an Error carrying the server's message (or fallback).
export async function apiJSON<T>(path:string,method:Method="GET",body?:unknown,fallback="Request failed",opts?:{csrf?:boolean;signal?:AbortSignal}):Promise<T>{
 const r=await apiFetch(path,method,body,opts);
 const data=await r.json();
 if(!r.ok)throw Error(data.error||fallback);
 return data as T;
}
