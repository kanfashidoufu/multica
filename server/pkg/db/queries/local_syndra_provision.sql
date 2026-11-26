-- name: ListSyndraLoginIdentities :many
-- Also inspect existing bindings on the account holding the authoritative email.
-- A collision needs review; never move a binding from one account to another.
SELECT x.* FROM user_external_identity x
WHERE x.provider = 'lark' AND (
    (x.tenant_key = @tenant_key AND (x.open_id = @open_id::text
        OR x.external_user_id = @external_user_id::text
        OR (@union_id::text <> '' AND x.union_id = @union_id::text)))
    OR x.user_id = sqlc.narg(email_user_id)::uuid
)
LIMIT 2;

-- name: BindSyndraLoginIdentity :exec
INSERT INTO user_external_identity (
    user_id, provider, tenant_key, external_user_id, open_id, union_id,
    email, name, raw_profile, last_synced_at
) VALUES (@user_id, 'lark', @tenant_key, @external_user_id, @open_id,
    sqlc.narg(union_id), sqlc.narg(email), @name, @raw_profile, now());
