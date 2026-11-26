-- name: FillLarkPlaceholderEmail :one
-- Only promote an already bound login's unchanged placeholder. The unique
-- account-email constraint arbitrates concurrent claims from distinct users.
UPDATE "user" AS u
SET email = sqlc.arg('enterprise_email'), updated_at = now()
WHERE u.id = sqlc.arg('user_id') AND u.email = sqlc.arg('placeholder_email')
  AND u.email LIKE '%@lark.local'
  AND EXISTS (
    SELECT 1 FROM user_external_identity AS x
    WHERE x.user_id = u.id AND x.provider = 'lark'
      AND x.tenant_key = sqlc.arg('tenant_key') AND x.open_id = sqlc.arg('open_id')::text
  )
RETURNING u.*;
