-- One supplier credential has one fixed request tier within an enabled group.
-- Reject conflicting old data explicitly; never choose a tier or rewrite it.
ALTER TABLE account_groups
    ADD CONSTRAINT account_groups_request_speed_single_assignment_check
    CHECK (jsonb_array_length(request_speed_tiers) <= 1);
