-- What a device is for. Additive: older app versions ignore both columns.
-- role: access_point (default) or router (also the gateway). dhcp_host marks the device
-- whose DHCP lease file names Wi-Fi clients; leases from other devices are ignored.
ALTER TABLE devices ADD COLUMN IF NOT EXISTS role TEXT NOT NULL DEFAULT 'access_point' CHECK (role IN ('access_point','router'));
ALTER TABLE devices ADD COLUMN IF NOT EXISTS dhcp_host BOOLEAN NOT NULL DEFAULT FALSE;
