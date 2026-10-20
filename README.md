# OpenManage

Lightweight, self-hosted management for OpenWrt devices. Go control plane, PostgreSQL storage, embedded React UI, and Docker-first deployment.

> **Early development:** Device inventory is the first milestone. The OpenWrt agent, authentication, configuration deployment, and firmware management are not implemented yet. Do not expose this service to untrusted networks.

## Goals

- One Go process serves the API and embedded frontend.
- PostgreSQL is the only required supporting service.
- Redis remains optional until a separate queue backend is useful.
- Docker Compose with conservative resource limits.
- Secure device enrollment and local rollback before managing real routers.

## Roadmap

1. Foundation: API, schema, inventory UI, health endpoint, Docker.
2. Authentication, roles and audit events.
3. OpenWrt enrollment and lightweight device agent.
4. Configuration templates, diffs, versioning and rollback.
5. Monitoring, alerts, firmware management and topology.
6. Optional RADIUS, IPAM, VPN/PKI and captive portal integrations.

## Security

This is an early-development build. Do not expose it directly to the internet. Change the database password and implement authentication before using it as a management service.
