-- name: CreateEnv :one
INSERT INTO environments (
  id,
  name,
  created_at
) VALUES (
  gen_random_uuid(),
  $1,
  clock_timestamp()
) RETURNING *;

-- name: GetEnv :one
SELECT * FROM environments WHERE id = $1;

-- name: ListEnvs :many
SELECT * FROM environments ORDER BY created_at ASC;

-- name: UpdateEnv :one
UPDATE environments SET name = $2 WHERE id = $1
RETURNING *;

-- name: DeleteEnv :one
DELETE FROM environments WHERE id = $1
RETURNING *;
