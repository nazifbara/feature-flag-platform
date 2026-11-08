-- name: CreateUser :one
INSERT INTO users (
    id,
    email,
    firstname,
    lastname,
    created_at,
    updated_at
)
VALUES (
    $1,
    $2,
    $3,
    $4,
    NOW(),
    NOW()
) RETURNING *;
