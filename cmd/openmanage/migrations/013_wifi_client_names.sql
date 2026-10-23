-- Hostname learned from the router's DHCP leases. Additive: older app versions ignore it.
ALTER TABLE wifi_clients ADD COLUMN IF NOT EXISTS dhcp_name TEXT NOT NULL DEFAULT '';
