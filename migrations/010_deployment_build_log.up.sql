-- The log of a deployment's build (M5), kept as the tail of what BuildKit
-- printed. It is a column of its own and the deployment queries do not select it:
-- every page of a project lists twenty deployments, and a log is up to 256 KiB.
ALTER TABLE deployments ADD COLUMN build_log TEXT NOT NULL DEFAULT '';
