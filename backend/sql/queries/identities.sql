-- name: CreateAuth :one
INSERT INTO identities (id, email, passwordhash, created_at, updated_at)
VALUES (
    gen_random_uuid(),
    $1,
    $2,
    now(),
    now()
) RETURNING *;
