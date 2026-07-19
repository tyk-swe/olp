def without_documentation:
  walk(
    if type == "object" then
      del(.description, .summary, .title, .externalDocs, .example, .examples, .tags)
    else
      .
    end
  )
  | del(.info);

def methods:
  ["get", "put", "post", "delete", "options", "head", "patch", "trace"];

def operations($document):
  [
    $document.paths
    | to_entries[] as $path
    | $path.value
    | to_entries[] as $method
    | select(methods | index($method.key))
    | {
        path: $path.key,
        method: $method.key,
        operation: $method.value,
        parameters: (($path.value.parameters // []) + ($method.value.parameters // []))
      }
  ];

def operation($entries; $path; $method):
  (first($entries[] | select(.path == $path and .method == $method)) // null);

def parameter($parameters; $location; $name):
  (first($parameters[] | select(.in == $location and .name == $name)) // null);

def parameter_identity($parameter):
  "\($parameter.in):\($parameter.name)";

def pointer($path):
  "#/" + (
    $path
    | map(tostring | gsub("~"; "~0") | gsub("/"; "~1"))
    | join("/")
  );

def critical_schema_keys:
  [
    "$ref",
    "type",
    "format",
    "enum",
    "const",
    "default",
    "required",
    "pattern",
    "minimum",
    "exclusiveMinimum",
    "maximum",
    "exclusiveMaximum",
    "multipleOf",
    "minLength",
    "maxLength",
    "minItems",
    "maxItems",
    "uniqueItems",
    "minProperties",
    "maxProperties",
    "additionalProperties",
    "allOf",
    "oneOf",
    "anyOf",
    "not",
    "discriminator",
    "readOnly",
    "writeOnly"
  ];

def critical_paths($document):
  [
    $document
    | paths as $path
    | select(
        ($path | length) > 0
        and ($path[-1] | type) == "string"
        and (critical_schema_keys | index($path[-1]))
        and (($path | index("parameters")) == null)
      )
    | $path
  ];

def has_parent($document; $path):
  ($path | length) > 0
  and (($document | getpath($path[0:-1])) != null);

def critical_values_equal($path; $baseline_value; $current_value):
  if (($path[-1] == "required" or $path[-1] == "enum" or $path[-1] == "type")
      and ($baseline_value | type) == "array"
      and ($current_value | type) == "array") then
    ($baseline_value | sort) == ($current_value | sort)
  else
    $baseline_value == $current_value
  end;

($baseline[0]) as $baseline_raw
| ($current[0]) as $current_raw
| ($baseline_raw | without_documentation) as $baseline_api
| ($current_raw | without_documentation) as $current_api
| (operations($baseline_api)) as $baseline_operations
| (operations($current_api)) as $current_operations
| [
    if ($current_api | contains($baseline_api)) then
      empty
    else
      "the current API removed or changed a frozen v1 operation, parameter, response, security declaration, or schema value"
    end,

    if (($baseline_raw.info.version | split(".")[0]) == ($current_raw.info.version | split(".")[0])) then
      empty
    else
      "the current API changed the frozen baseline major without retaining the v1 surface"
    end,

    if (($baseline_api.security // "__absent__") == ($current_api.security // "__absent__")) then
      empty
    else
      "top-level OpenAPI security changed"
    end,

    ($baseline_operations[] as $baseline_operation
    | operation($current_operations; $baseline_operation.path; $baseline_operation.method) as $current_operation
    | if $current_operation == null then
        "removed operation \($baseline_operation.method | ascii_upcase) \($baseline_operation.path)"
      elif (($baseline_operation.operation.security // "__absent__") != ($current_operation.operation.security // "__absent__")) then
        "security changed for \($baseline_operation.method | ascii_upcase) \($baseline_operation.path)"
      else
        empty
      end),

    ($baseline_operations[] as $baseline_operation
    | operation($current_operations; $baseline_operation.path; $baseline_operation.method) as $current_operation
    | select($current_operation != null)
    | $current_operation.parameters[]
    | select(.required == true)
    | select(parameter($baseline_operation.parameters; .in; .name) == null)
    | "new required parameter \(parameter_identity(.)) on \($baseline_operation.method | ascii_upcase) \($baseline_operation.path)"),

    ($baseline_operations[] as $baseline_operation
    | operation($current_operations; $baseline_operation.path; $baseline_operation.method) as $current_operation
    | select($current_operation != null)
    | $baseline_operation.parameters[] as $baseline_parameter
    | parameter($current_operation.parameters; $baseline_parameter.in; $baseline_parameter.name) as $current_parameter
    | select($current_parameter != null)
    | select(
        (($baseline_parameter.schema // "__absent__") | without_documentation)
        != (($current_parameter.schema // "__absent__") | without_documentation)
      )
    | "schema changed for parameter \(parameter_identity($baseline_parameter)) on \($baseline_operation.method | ascii_upcase) \($baseline_operation.path)"),

    ($baseline_operations[] as $baseline_operation
    | operation($current_operations; $baseline_operation.path; $baseline_operation.method) as $current_operation
    | select($current_operation != null)
    | select(
        ($baseline_operation.operation.requestBody // null) == null
        and ($current_operation.operation.requestBody.required // false) == true
      )
    | "new required request body on \($baseline_operation.method | ascii_upcase) \($baseline_operation.path)"),

    (critical_paths($current_api)[] as $path
    | select(has_parent($baseline_api; $path))
    | select(critical_values_equal(
        $path;
        ($baseline_api | getpath($path));
        ($current_api | getpath($path))
      ) | not)
    | "incompatible schema constraint changed at \(pointer($path))")
  ]
| unique
| { compatible: (length == 0), violations: . }
