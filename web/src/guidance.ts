export const statusLabel={online:"Online",offline:"Offline",unknown:"Not enrolled",degraded:"Degraded"};
export const enrollGuidance:Record<string,string>={
 encryption_key_missing:"The server has no OPENMANAGE_ENCRYPTION_KEY. Generate one with `openssl rand -base64 32`, add it to .env, back it up, and restart OpenManage.",
 encryption_key_invalid:"OPENMANAGE_ENCRYPTION_KEY must be the base64 encoding of exactly 32 random bytes. Regenerate it with `openssl rand -base64 32` before enrolling devices.",
 address_not_private:"Enrollment only accepts private IP addresses. Remove this inventory record and add the device again using its private IP (for example 192.168.1.1), not a hostname or public address.",
 invalid_private_key:"The key could not be read. Paste a complete, unencrypted OpenSSH or PEM private key, including the BEGIN and END lines.",
 device_unreachable:"Nothing answered on TCP/22. Check that the device is powered on, SSH (dropbear) is running, and this server can reach the address.",
 host_key_mismatch:"The device presented a different host key. Re-check the fingerprint from a trusted console on the router. If the key really changed, find out why before trusting it.",
 key_not_verified:"The router let OpenManage in without checking the key, which usually means root has no password. Set a root password (run passwd on the router or use LuCI), then enroll again so the key itself is verified.",
 ssh_auth_failed:"The device rejected the key. Make sure its public key is in /etc/dropbear/authorized_keys on the router and the username is correct.",
 invalid_request:"Fill in the username, the private key, and a fingerprint starting with SHA256:.",
};
// Mirrors the server's net.ParseIP(...).IsPrivate() rule; anything it cannot confirm gets the warning.
export function looksEnrollable(address:string){
 const a=address.trim().toLowerCase();
 const v4=a.split(".");
 if(v4.length===4&&v4.every(p=>/^\d{1,3}$/.test(p)&&Number(p)<=255)){const [x,y]=v4.map(Number);return x===10||(x===172&&y>=16&&y<=31)||(x===192&&y===168)}
 return /^f[cd][0-9a-f]{2}:[0-9a-f:]*$/.test(a)&&a.split("::").length<=2&&a.split(":").every(g=>g.length<=4)
}
