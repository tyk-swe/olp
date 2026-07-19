def keys_are($expected):
  (keys | sort) == ($expected | sort);

def keys_within($allowed):
  . as $object
  | all($object | keys[]; . as $key | $allowed | index($key) != null);

def has_all($required):
  . as $object
  | all($required[]; . as $key | $object | has($key));

def valid_uuid:
  type == "string"
  and test("^[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}$");

def valid_literal($schema; $policy):
  . as $value
  | if type == "boolean" or type == "null" then true
    elif type == "number" then
      floor == .
      and . >= $schema."$defs".literal.oneOf[1].minimum
      and . <= $schema."$defs".literal.oneOf[1].maximum
    elif type == "string" then
      length <= $schema."$defs".literal.oneOf[2].maxLength
      and utf8bytelength <= $policy.bounds.string_bytes_per_value
    else false
    end;

def valid_operand($schema; $policy):
  . as $operand
  | type == "object"
  and if has("field") then
    keys_are(["field"])
    and ($operand.field | type == "string" and test($schema."$defs".field_path.pattern))
  elif has("literal") then
    keys_are(["literal"])
    and ($operand.literal | valid_literal($schema; $policy))
  elif has("decimal") then
    keys_are(["decimal"])
    and ($operand.decimal | type == "string"
      and test($schema."$defs".operand.oneOf[2].properties.decimal.pattern))
  else false
  end;

def valid_expression($schema; $policy):
  . as $expression
  | type == "object"
  and if has("const") then
    keys_are(["const"]) and ($expression.const | type == "boolean")
  elif has("not") then
    keys_are(["not"]) and ($expression.not | valid_expression($schema; $policy))
  elif has("all") then
    keys_are(["all"])
    and ($expression.all | type == "array" and length >= 1 and length <= 64)
    and all($expression.all[]; valid_expression($schema; $policy))
  elif has("any") then
    keys_are(["any"])
    and ($expression.any | type == "array" and length >= 1 and length <= 64)
    and all($expression.any[]; valid_expression($schema; $policy))
  elif has("compare") then
    keys_are(["compare", "left", "right"])
    and ($schema."$defs".expression.oneOf[4].properties.compare.enum
      | index($expression.compare) != null)
    and ($expression.left | valid_operand($schema; $policy))
    and ($expression.right | valid_operand($schema; $policy))
  elif has("present") then
    keys_are(["present"])
    and ($expression.present | type == "string" and test($schema."$defs".field_path.pattern))
  elif has("in") then
    keys_are(["in", "values"])
    and ($expression.in | valid_operand($schema; $policy))
    and ($expression.values | type == "array" and length >= 1 and length <= 256
      and length == (unique | length))
    and all($expression.values[]; valid_literal($schema; $policy))
  else false
  end;

def parameter_is_valid($policy; $action):
  ([$policy.action_semantics.parameter_groups
    | to_entries[]
    | select(.value | index($action.kind) != null)
    | .key]) as $groups
  | (["name", "integer_value", "field_path", "candidate_ids"]
    | map(. as $key | select($action | has($key)))) as $present
  | ($groups | length) == 1
  and (if $groups[0] == "none" then ($present | length) == 0
    else $present == [$groups[0]] end);

def valid_action($schema; $policy; $phase):
  . as $action
  | type == "object"
  and keys_within($schema."$defs".action.properties | keys)
  and has_all($schema."$defs".action.required)
  and ($schema."$defs".action.properties.kind.enum | index($action.kind) != null)
  and ($policy.phases[$phase].actions | index($action.kind) != null)
  and ($action.reason_code | type == "string" and test($schema."$defs".reason_code.pattern)
    and utf8bytelength <= $policy.bounds.reason_code_bytes)
  and ($action.enforcement == "hard" or $action.enforcement == "advisory")
  and (if $action.enforcement == "advisory"
    then ($policy.action_semantics.advisory_allowed_actions | index($action.kind) != null)
    else true end)
  and (if $action | has("name")
    then ($action.name | type == "string" and test($schema."$defs".action.properties.name.pattern))
    else true end)
  and (if $action | has("integer_value")
    then ($action.integer_value | type == "number" and floor == .
      and . >= $schema."$defs".action.properties.integer_value.minimum
      and . <= $schema."$defs".action.properties.integer_value.maximum)
    else true end)
  and (if $action | has("field_path")
    then ($action.field_path | type == "string" and test($schema."$defs".field_path.pattern))
    else true end)
  and (if $action | has("candidate_ids")
    then ($action.candidate_ids | type == "array"
      and length <= $schema."$defs".action.properties.candidate_ids.maxItems
      and length == (unique | length))
      and all($action.candidate_ids[]; valid_uuid)
    else true end)
  and parameter_is_valid($policy; $action);

def valid_rule($schema; $policy; $phase):
  . as $rule
  | type == "object"
  and keys_are(["rule_id", "when", "actions"])
  and ($rule.rule_id | type == "string" and test($schema."$defs".rule.properties.rule_id.pattern))
  and ($rule.when | valid_expression($schema; $policy))
  and ($rule.actions | type == "array" and length >= 1
    and length <= $schema."$defs".rule.properties.actions.maxItems)
  and all($rule.actions[]; valid_action($schema; $policy; $phase));

def valid_program($schema; $policy):
  . as $program
  | type == "object"
  and keys_are($schema.required)
  and $program.schema_version == $schema.properties.schema_version.const
  and $program.language_version == $schema.properties.language_version.const
  and ($program.program_id | type == "string" and test($schema.properties.program_id.pattern))
  and ($program.revision_id | valid_uuid)
  and ($schema.properties.phase.enum | index($program.phase) != null)
  and ($program.rules | type == "array" and length <= $schema.properties.rules.maxItems)
  and ([$program.rules[].rule_id] | length == (unique | length))
  and all($program.rules[]; valid_rule($schema; $policy; $program.phase))
  and ($program.default_actions | type == "array"
    and length <= $schema.properties.default_actions.maxItems)
  and all($program.default_actions[]; valid_action($schema; $policy; $program.phase));

def lookup_field($input; $path):
  reduce ($path | split("."))[] as $segment
    ({found: true, value: $input, kind: "field"};
      if .found
        and (.value | type) == "object"
        and (.value | has($segment))
      then .value[$segment] as $value
        | {found: true, value: $value, kind: "field"}
      else {found: false, kind: "field"}
      end);

def operand_value($input; $operand):
  if $operand.field != null then lookup_field($input; $operand.field)
  elif $operand.decimal != null then {found: true, value: $operand.decimal, kind: "decimal"}
  else {found: true, value: $operand.literal, kind: "literal"}
  end;

def decimal_parts($value):
  $value
  | capture("^(?<sign>-?)(?<integer>0|[1-9][0-9]{0,19})[.](?<fraction>[0-9]{9})$")
  | ((("00000000000000000000" + .integer) | .[-20:]) + .fraction) as $magnitude
  | {negative: (.sign == "-"), magnitude: $magnitude};

def decimal_order($left; $right):
  decimal_parts($left) as $left_parts
  | decimal_parts($right) as $right_parts
  | if $left_parts.negative != $right_parts.negative then
      if $left_parts.negative then -1 else 1 end
    elif $left_parts.magnitude == $right_parts.magnitude then 0
    elif $left_parts.negative then
      if $left_parts.magnitude > $right_parts.magnitude then -1 else 1 end
    else
      if $left_parts.magnitude < $right_parts.magnitude then -1 else 1 end
    end;

def comparison_result($order; $operator):
  if $operator == "eq" then $order == 0
  elif $operator == "ne" then $order != 0
  elif $operator == "lt" then $order < 0
  elif $operator == "lte" then $order <= 0
  elif $operator == "gt" then $order > 0
  else $order >= 0
  end;

def compare_values($left; $right; $operator; $decimal_pattern):
  if $left.kind == "decimal" or $right.kind == "decimal" then
    if ($left.value | type == "string" and test($decimal_pattern))
      and ($right.value | type == "string" and test($decimal_pattern))
    then {ok: true, value: comparison_result(decimal_order($left.value; $right.value); $operator)}
    else {ok: false, error: "type_mismatch"}
    end
  elif ($left.value | type) != ($right.value | type) then
    if $operator == "eq" then {ok: true, value: false}
    elif $operator == "ne" then {ok: true, value: true}
    else {ok: false, error: "type_mismatch"}
    end
  else
    (if $left.value < $right.value then -1
      elif $left.value > $right.value then 1
      else 0 end) as $order
    | {ok: true, value: comparison_result($order; $operator)}
  end;

def eval_expression($schema; $input; $expression):
  if $expression.const != null then {ok: true, value: $expression.const}
  elif $expression.not != null then
    eval_expression($schema; $input; $expression.not) as $result
    | if $result.ok then {ok: true, value: ($result.value | not)} else $result end
  elif $expression.all != null then
    reduce $expression.all[] as $child
      ({ok: true, value: true};
        if (.ok | not) or (.value | not) then .
        else eval_expression($schema; $input; $child)
        end)
  elif $expression.any != null then
    reduce $expression.any[] as $child
      ({ok: true, value: false};
        if (.ok | not) or .value then .
        else eval_expression($schema; $input; $child)
        end)
  elif $expression.compare != null then
    operand_value($input; $expression.left) as $left
    | operand_value($input; $expression.right) as $right
    | if ($left.found | not) or ($right.found | not)
      then {ok: false, error: "missing_required_input"}
      else compare_values(
        $left;
        $right;
        $expression.compare;
        $schema."$defs".operand.oneOf[2].properties.decimal.pattern)
      end
  elif $expression.present != null then
    lookup_field($input; $expression.present)
    | {ok: true, value: .found}
  else
    operand_value($input; $expression.in) as $candidate
    | if ($candidate.found | not) then {ok: false, error: "missing_required_input"}
      else {ok: true, value: any($expression.values[]; . == $candidate.value)}
      end
  end;

def evaluate_program($schema; $program; $input):
  reduce $program.rules[] as $rule
    ({ok: true, actions: [], matched_rules: [], terminal: false};
      if (.ok | not) or .terminal then .
      else . as $state
        | eval_expression($schema; $input; $rule.when) as $condition
        | if ($condition.ok | not) then
            {ok: false, error: $condition.error}
          elif ($condition.value | not) then $state
          else
            ($state.actions + $rule.actions) as $actions
            | {
                ok: true,
                actions: $actions,
                matched_rules: ($state.matched_rules + [$rule.rule_id]),
                terminal: any($actions[]; .kind == "deny" or .kind == "stop")
              }
          end
      end)
  | if (.ok | not) then
      {result: "evaluation_error", error: .error, disposition: "deny"}
    elif (.matched_rules | length) == 0 then
      {result: "actions", actions: $program.default_actions, matched_rules: []}
    else
      {result: "actions", actions: .actions, matched_rules: .matched_rules}
    end;

. as $golden
| $policy[0] as $policy_contract
| $schema[0] as $program_schema
| $golden.contract_id == "olp.enterprise.policy-v1-golden.v1"
and $golden.schema_version == 1
and $golden.language_version == $policy_contract.identity.language_version
and ($program_schema.additionalProperties == false)
and ($program_schema."$defs".action.additionalProperties == false)
and ($program_schema.properties.rules.maxItems == $policy_contract.bounds.policy_nodes_per_program)
and ($program_schema.properties.default_actions.maxItems == $policy_contract.bounds.output_actions_per_phase)
and ($program_schema."$defs".literal.oneOf[2].maxLength == $policy_contract.bounds.string_bytes_per_value)
and ($golden.vectors | type == "array" and length >= ($policy_contract.phase_order | length))
and ([$golden.vectors[].id] | length == (unique | length))
and ([$golden.vectors[].program.phase] | unique | sort == ($policy_contract.phase_order | sort))
and all($golden.vectors[];
  keys_are(["id", "program", "input", "expected"])
  and (.id | type == "string" and test("^[a-z][a-z0-9-]{0,63}$"))
  and (.input | type == "object")
  and (.program | valid_program($program_schema; $policy_contract))
  and (evaluate_program($program_schema; .program; .input) == .expected))
and ($golden.negative_vectors | type == "array" and length > 0)
and all($golden.negative_vectors[];
  .id == "decimal-negative-zero-is-not-canonical"
  and .schema_path == "#/$defs/operand/oneOf/2/properties/decimal"
  and .expected == "schema_rejection"
  and (.value | type == "string")
  and (.value | test($program_schema."$defs".operand.oneOf[2].properties.decimal.pattern) | not))
