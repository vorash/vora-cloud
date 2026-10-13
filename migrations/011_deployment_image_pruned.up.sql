-- When the image of a deployment was removed from the registry to save space (M5
-- retention). A deployment whose image is gone cannot be rolled back to, and the
-- service reads this column on every deployment it lists, so apply this migration
-- before the binary that reads it.
ALTER TABLE deployments ADD COLUMN image_pruned_at TIMESTAMPTZ;
