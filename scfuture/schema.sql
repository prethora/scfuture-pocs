-- scfuture schema — for reference when migrating to Postgres
-- Currently the coordinator uses in-memory state (Layer 4.2)

CREATE TABLE machines (
    machine_id      TEXT PRIMARY KEY,
    address         TEXT NOT NULL,
    public_address  TEXT,
    status          TEXT NOT NULL DEFAULT 'active',
    disk_total_mb   BIGINT NOT NULL DEFAULT 0,
    disk_used_mb    BIGINT NOT NULL DEFAULT 0,
    ram_total_mb    BIGINT NOT NULL DEFAULT 0,
    ram_used_mb     BIGINT NOT NULL DEFAULT 0,
    active_agents   INTEGER DEFAULT 0,
    max_agents      INTEGER DEFAULT 200,
    last_heartbeat  TIMESTAMP
);

CREATE TABLE users (
    user_id         TEXT PRIMARY KEY,
    status          TEXT NOT NULL DEFAULT 'registered',
    primary_machine TEXT REFERENCES machines(machine_id),
    drbd_port       INTEGER UNIQUE,
    image_size_mb   INTEGER DEFAULT 512,
    error           TEXT,
    created_at      TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE bipods (
    user_id         TEXT NOT NULL REFERENCES users(user_id),
    machine_id      TEXT NOT NULL REFERENCES machines(machine_id),
    role            TEXT NOT NULL,
    drbd_minor      INTEGER,
    loop_device     TEXT,
    created_at      TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (user_id, machine_id)
);

CREATE TABLE provisioning_log (
    id              SERIAL PRIMARY KEY,
    user_id         TEXT NOT NULL REFERENCES users(user_id),
    state           TEXT NOT NULL,
    details         TEXT,
    created_at      TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);
