-- The RADIUS server's TLS identity for EAP-TTLS: a private CA that Wi-Fi clients are
-- told to trust, and a server certificate it signs. One row; private keys are
-- AES-256-GCM encrypted with the application key.
CREATE TABLE IF NOT EXISTS radius_certificate (
    id SMALLINT PRIMARY KEY CHECK (id = 1),
    ca_cert BYTEA NOT NULL,
    ca_key_ciphertext BYTEA NOT NULL,
    server_cert BYTEA NOT NULL,
    server_key_ciphertext BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    renewed_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
