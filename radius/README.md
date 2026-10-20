# RADIUS lab service

This is an **opt-in lab integration**, separate from the default OpenManage application and database. It uses the maintained FreeRADIUS 3.2 server image rather than implementing the RADIUS protocol in the web API. Go remains the OpenManage application language; using FreeRADIUS avoids making a new protocol implementation a prerequisite for the first working lab.

## Start and test locally

From the repository root, with Docker Compose installed:

```sh
docker compose -f compose.radius.yaml up -d
bash scripts/test-radius.sh
```

The test uses a disposable lab identity (`testuser` / `testpass`) and the loopback-only client secret `testing123`. These values are intentionally unsuitable for any network-facing deployment. The RADIUS service is not started by the normal `docker compose up` command.

Stop the lab service with:

```sh
docker compose -f compose.radius.yaml down
```

## Connect an OpenWrt access point

Do not expose the sample configuration as-is. For a real AP:

1. Generate a unique long random shared secret for each AP.
2. Edit `radius/clients.conf`: add a separate `client` stanza using that AP's exact management IP and the unique secret. Never add the whole LAN subnet as a client.
3. Replace or remove the lab test identity in `radius/authorize`. Do not reuse the example username/password.
4. Set `RADIUS_BIND_IP` in the shell environment or `.env` to the server's specific LAN IP, then start the service. Do not bind RADIUS to all interfaces unless a firewall explicitly limits access.
5. Permit UDP 1812 from the configured AP addresses. Permit UDP 1813 only if accounting is configured and needed. Do not forward these ports from the internet.
6. Configure the AP's RADIUS server address, authentication port, and matching shared secret in OpenWrt. Verify the configured client IP is the source address actually seen by the server.
7. Run a test authentication with a non-production account before changing a live SSID.

The included test validates basic RADIUS PAP authentication from a local test client. It does **not** validate WPA-Enterprise EAP/PEAP, production certificate deployment, accounting persistence, a user-management UI, or integration with OpenManage's PostgreSQL user table. The image's default EAP certificates are not production credentials. Those are explicit follow-on tasks; do not use this lab as a production RADIUS deployment.

## Recovery and isolation

This service is optional and does not modify OpenWrt configuration. To back out, stop it with the command above and remove any test AP settings. Keep the normal OpenManage Compose stack unchanged while evaluating the lab.
