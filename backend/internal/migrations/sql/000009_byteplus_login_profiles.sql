ALTER TABLE refresh_profiles
    ADD COLUMN login_identity TEXT NOT NULL DEFAULT '',
    ADD COLUMN login_secret TEXT NOT NULL DEFAULT '';
