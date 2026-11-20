CREATE INDEX retained_mutations_unresolved_resource
 ON olp.retained_mutations(resource_id)
 WHERE state IN ('dispatched','submission_unknown');
