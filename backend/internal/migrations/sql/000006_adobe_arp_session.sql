ALTER TABLE provider_accounts
    ADD COLUMN arp_session_token TEXT NOT NULL DEFAULT '';

ALTER TABLE refresh_profiles
    ADD COLUMN arp_session_token TEXT NOT NULL DEFAULT '';
