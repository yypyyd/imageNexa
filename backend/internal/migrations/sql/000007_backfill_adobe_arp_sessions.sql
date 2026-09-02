-- Adobe's SherlockSdk creates the initial x-arp-session-id locally as padded
-- base64 of compact JSON containing a random v4 session UUID. Backfill legacy
-- Adobe rows so they can use the same base session immediately after upgrade.
UPDATE refresh_profiles
SET arp_session_token = encode(
    convert_to(format('{"sid":"%s"}', gen_random_uuid()::text), 'UTF8'),
    'base64'
)
WHERE pool = 'adobe'
  AND kind = 'adobe_cookie'
  AND BTRIM(arp_session_token) = '';

UPDATE provider_accounts AS account
SET arp_session_token = profile.arp_session_token
FROM refresh_profiles AS profile
WHERE account.pool = 'adobe'
  AND account.id = profile.id
  AND BTRIM(account.arp_session_token) = ''
  AND BTRIM(profile.arp_session_token) <> '';

UPDATE provider_accounts
SET arp_session_token = encode(
    convert_to(format('{"sid":"%s"}', gen_random_uuid()::text), 'UTF8'),
    'base64'
)
WHERE pool = 'adobe'
  AND BTRIM(arp_session_token) = '';
