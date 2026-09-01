-- Retire the five video models removed from the closed public catalog.
-- Immutable dispatch history wins over physical deletion: a referenced route
-- remains as a disabled tombstone, while unused routes and logical models are
-- removed completely.

UPDATE logical_models
SET enabled = FALSE, updated_at = NOW()
WHERE id IN (
    'luma-ray',
    'runway-gen-4-turbo',
    'runway-gen-4.5',
    'veo-3.1',
    'veo-3.1-lite'
);

UPDATE model_routes
SET enabled = FALSE, updated_at = NOW()
WHERE logical_model_id IN (
    'luma-ray',
    'runway-gen-4-turbo',
    'runway-gen-4.5',
    'veo-3.1',
    'veo-3.1-lite'
);

DELETE FROM account_model_routes
WHERE model_route_id IN (
    SELECT id
    FROM model_routes
    WHERE logical_model_id IN (
        'luma-ray',
        'runway-gen-4-turbo',
        'runway-gen-4.5',
        'veo-3.1',
        'veo-3.1-lite'
    )
);

DELETE FROM model_routes AS route
WHERE route.logical_model_id IN (
    'luma-ray',
    'runway-gen-4-turbo',
    'runway-gen-4.5',
    'veo-3.1',
    'veo-3.1-lite'
)
AND NOT EXISTS (
    SELECT 1
    FROM dispatch_attempts AS attempt
    WHERE attempt.model_route_id = route.id
);

DELETE FROM logical_models AS logical
WHERE logical.id IN (
    'luma-ray',
    'runway-gen-4-turbo',
    'runway-gen-4.5',
    'veo-3.1',
    'veo-3.1-lite'
)
AND NOT EXISTS (
    SELECT 1
    FROM model_routes AS route
    WHERE route.logical_model_id = logical.id
);
