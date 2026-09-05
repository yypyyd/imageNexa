-- Provider-account deletion cascades through account_quota_buckets. Reservations
-- belong to those buckets, so retaining a reservation while deleting its bucket
-- is impossible and previously made every used account undeletable.
ALTER TABLE quota_reservations
    DROP CONSTRAINT IF EXISTS quota_reservations_quota_bucket_id_fkey;

ALTER TABLE quota_reservations
    ADD CONSTRAINT quota_reservations_quota_bucket_id_fkey
    FOREIGN KEY (quota_bucket_id)
    REFERENCES account_quota_buckets(id)
    ON DELETE CASCADE;
