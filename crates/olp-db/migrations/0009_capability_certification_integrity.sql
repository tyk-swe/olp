-- Capability provenance is security-relevant routing evidence. Normalize any
-- pre-constraint development data, then make it impossible to claim certified
-- provenance without a timestamp (or attach a timestamp to a declaration).
UPDATE model_capabilities
SET certified_at = now()
WHERE source = 'certified' AND certified_at IS NULL;

UPDATE model_capabilities
SET certified_at = NULL
WHERE source <> 'certified' AND certified_at IS NOT NULL;

ALTER TABLE model_capabilities
    ADD CONSTRAINT model_capabilities_certification_evidence_check
    CHECK ((source = 'certified') = (certified_at IS NOT NULL));
