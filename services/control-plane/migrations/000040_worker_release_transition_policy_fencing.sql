-- Worker release transitions are immutable audit history. Ensure an upgrade
-- does not preserve a Policy whose latest Transition projects a different
-- state before fencing all future inserts.

DO $$
BEGIN
  IF EXISTS (
    SELECT 1
    FROM worker_release_policies AS policy
    LEFT JOIN LATERAL (
      SELECT transition.*
      FROM worker_release_transitions AS transition
      WHERE transition.execution_target_id = policy.execution_target_id
      ORDER BY transition.policy_version DESC
      LIMIT 1
    ) AS latest ON TRUE
    WHERE latest.id IS NULL
       OR latest.tenant_id IS DISTINCT FROM policy.tenant_id
       OR latest.policy_version IS DISTINCT FROM policy.policy_version
       OR latest.to_promoted_revision_id IS DISTINCT FROM policy.promoted_revision_id
       OR latest.to_canary_revision_id IS DISTINCT FROM policy.canary_revision_id
       OR latest.canary_percent IS DISTINCT FROM policy.canary_percent
  ) THEN
    RAISE EXCEPTION 'Worker release Policy does not match its latest immutable Transition'
      USING ERRCODE = '23514';
  END IF;
END
$$;

CREATE OR REPLACE FUNCTION enforce_worker_release_transition_policy_match()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NOT EXISTS (
    SELECT 1
    FROM worker_release_policies AS policy
    WHERE policy.tenant_id = NEW.tenant_id
      AND policy.execution_target_id = NEW.execution_target_id
      AND policy.policy_version = NEW.policy_version
      AND policy.promoted_revision_id = NEW.to_promoted_revision_id
      AND policy.canary_revision_id IS NOT DISTINCT FROM NEW.to_canary_revision_id
      AND policy.canary_percent = NEW.canary_percent
  ) THEN
    RAISE EXCEPTION 'Worker release Transition does not match the current Policy state'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_worker_release_transitions_policy_match
  ON worker_release_transitions;

CREATE TRIGGER trg_worker_release_transitions_policy_match
BEFORE INSERT ON worker_release_transitions
FOR EACH ROW EXECUTE FUNCTION enforce_worker_release_transition_policy_match();
