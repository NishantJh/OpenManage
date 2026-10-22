export type Device={id:string;name:string;address:string;model:string;firmware:string;status:"online"|"offline"|"unknown"|"degraded";last_seen_at:string;enrollable:boolean};
export type AuditEvent={id:number;username:string;action:string;resource_type:string;resource_id:string;details:Record<string,unknown>;created_at:string};
export type HealthResult={name:string;label:string;status:"ok"|"error";output?:string;error?:string;duration_ms:number};
export type HealthResponse={device_id:string;checked_at:string;status:string;results:HealthResult[]};
export type MonitoringCheck={id:number;device_id:string;device_name:string;status:"online"|"offline"|"degraded"|"error";reason:string;started_at:string;finished_at:string;duration_ms:number};
export type MonitoringSettings={enabled:boolean;interval_seconds:number};
