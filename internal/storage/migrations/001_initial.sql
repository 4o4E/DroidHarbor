CREATE TABLE caller_credentials (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    secret_hash BLOB NOT NULL UNIQUE CHECK(length(secret_hash) = 32),
    created_at INTEGER NOT NULL,
    revoked_at INTEGER
) STRICT;
CREATE TABLE admin_sessions (
    id TEXT PRIMARY KEY,
    secret_hash BLOB NOT NULL UNIQUE CHECK(length(secret_hash) = 32),
    admin_credential_version TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL CHECK(expires_at > created_at),
    revoked_at INTEGER
) STRICT;
CREATE TABLE devices (
    id TEXT PRIMARY KEY,
    display_name TEXT NOT NULL,
    allocation_state TEXT NOT NULL CHECK(allocation_state IN ('available','leased','reclaiming','quarantined','maintenance')),
    generation INTEGER NOT NULL DEFAULT 0 CHECK(generation >= 0),
    maintenance_requested INTEGER NOT NULL DEFAULT 1 CHECK(maintenance_requested IN (0,1)),
    video_max_size INTEGER NOT NULL DEFAULT 1920 CHECK(video_max_size > 0),
    video_max_fps INTEGER NOT NULL DEFAULT 30 CHECK(video_max_fps > 0),
    video_bit_rate INTEGER NOT NULL DEFAULT 4000000 CHECK(video_bit_rate > 0),
    video_revision INTEGER NOT NULL DEFAULT 1 CHECK(video_revision > 0),
    last_error TEXT
) STRICT;
CREATE TABLE device_links (
    id TEXT PRIMARY KEY,
    device_id TEXT NOT NULL REFERENCES devices(id),
    kind TEXT NOT NULL CHECK(kind IN ('usb','wifi')),
    endpoint TEXT NOT NULL UNIQUE,
    observed_serial TEXT,
    identity_evidence TEXT,
    enabled INTEGER NOT NULL DEFAULT 0 CHECK(enabled IN (0,1)),
    last_seen_at INTEGER
) STRICT;
CREATE TABLE device_tags (
    device_id TEXT NOT NULL REFERENCES devices(id),
    tag TEXT NOT NULL CHECK(length(tag) > 0),
    PRIMARY KEY(device_id, tag)
) STRICT;
CREATE TABLE tasks (
    id TEXT PRIMARY KEY,
    caller_id TEXT NOT NULL REFERENCES caller_credentials(id),
    external_task_id TEXT NOT NULL,
    task_secret_hash BLOB NOT NULL UNIQUE CHECK(length(task_secret_hash) = 32),
    created_at INTEGER NOT NULL,
    UNIQUE(caller_id, external_task_id),
    UNIQUE(id, caller_id)
) STRICT;
CREATE TABLE leases (
    id TEXT PRIMARY KEY,
    task_id TEXT NOT NULL,
    caller_id TEXT NOT NULL,
    device_id TEXT NOT NULL REFERENCES devices(id),
    generation INTEGER NOT NULL CHECK(generation > 0),
    boot_id TEXT NOT NULL,
    status TEXT NOT NULL CHECK(status IN ('active','ended')),
    claimed_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL CHECK(expires_at > claimed_at),
    ended_at INTEGER,
    end_reason TEXT,
    claim_idempotency_key TEXT NOT NULL,
    claim_request_hash BLOB NOT NULL CHECK(length(claim_request_hash) = 32),
    FOREIGN KEY(task_id, caller_id) REFERENCES tasks(id, caller_id),
    UNIQUE(device_id, generation),
    UNIQUE(caller_id, claim_idempotency_key),
    CHECK((status = 'active' AND ended_at IS NULL AND end_reason IS NULL) OR
          (status = 'ended' AND ended_at IS NOT NULL AND end_reason IS NOT NULL))
) STRICT;
CREATE UNIQUE INDEX leases_active_device ON leases(device_id) WHERE status = 'active';
CREATE UNIQUE INDEX leases_active_task ON leases(task_id) WHERE status = 'active';
CREATE INDEX leases_caller ON leases(caller_id);
CREATE TABLE owned_resources (
    id TEXT PRIMARY KEY,
    device_id TEXT NOT NULL REFERENCES devices(id),
    owner_kind TEXT NOT NULL CHECK(owner_kind IN ('task','human-session','device-capture')),
    owner_id TEXT NOT NULL,
    boot_id TEXT NOT NULL,
    kind TEXT NOT NULL,
    locator TEXT NOT NULL,
    state TEXT NOT NULL CHECK(state IN ('pending','active','cleaning','cleaned','failed')),
    cleanup_error TEXT,
    created_at INTEGER NOT NULL
) STRICT;
CREATE INDEX owned_resources_device ON owned_resources(device_id, state);
CREATE TABLE operation_records (
    id TEXT PRIMARY KEY,
    actor_kind TEXT NOT NULL,
    actor_id TEXT NOT NULL,
    device_id TEXT REFERENCES devices(id),
    lease_id TEXT REFERENCES leases(id),
    kind TEXT NOT NULL,
    status TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    finished_at INTEGER,
    result_code TEXT,
    safe_details TEXT
) STRICT;
