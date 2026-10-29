-- name: CreateEnv :one
INSERT INTO environments (
  id,
  name
) VALUES (
  gen_random_uuid(), $1
) RETURNING *;

-- name: GetEnv :one
SELECT * FROM environments WHERE id = $1;
