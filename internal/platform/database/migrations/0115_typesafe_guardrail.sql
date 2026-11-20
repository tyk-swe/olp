ALTER TABLE olp.guardrails
    DROP CONSTRAINT guardrails_type_check,
    ADD CONSTRAINT guardrails_type_check CHECK (type IN (
        'builtin.regex','builtin.pii','builtin.secrets','builtin.keywords',
        'builtin.schema','builtin.tools','builtin.code_execution','builtin.tool_permission',
        'webhook','bedrock','azure.content_safety','azure.prompt_shields',
        'model_armor','lakera','presidio','model',
        'plugin','aim','akto','alice',
        'aporia','cato_networks','cisco_ai_defense','compresr',
        'conduct','crowdstrike_aidr','deepkeep','dynamoai',
        'enkryptai','generic_guardrail_api','grayswan','guardrails_ai',
        'headroom','hiddenlayer','ibm_guardrails','javelin',
        'lakera_v1','lasso','microsoft_purview','noma',
        'noma_v2','onyx','openai_moderation','ovalix',
        'pangea','panw_prisma_airs','pillar','prompt_security',
        'promptguard','qostodian_nexus','qualifire','repelloai',
        'rubrik','singulr','straiker','vigil_guard',
        'xecguard','zscaler_ai_guard','semantic_guard','decision_model','typesafe'
    ));
