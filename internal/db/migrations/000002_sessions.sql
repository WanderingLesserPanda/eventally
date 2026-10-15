-- +goose Up
-- 000002_sessions: DB-backed session store (survives app restarts in
-- docker-compose; the session cookie is the random id, never a secret
-- beyond it).
CREATE TABLE session (
    id           TEXT PRIMARY KEY,              -- 32 random bytes, base64url
    household_id BIGINT NOT NULL REFERENCES household(id) ON DELETE CASCADE,
    user_id      BIGINT NOT NULL REFERENCES "user"(id) ON DELETE CASCADE,
    csrf_token   TEXT NOT NULL,
    user_agent   TEXT NOT NULL DEFAULT '',
    ip           TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ NOT NULL,
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_session_household ON session (household_id);
CREATE INDEX idx_session_expires ON session (expires_at);
