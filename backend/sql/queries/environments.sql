-- name: CreateEnv :one
INSERT INTO environments (
  id,
  name
) VALUES (
  gen_random_uuid(), $1
) RETURNING *;
