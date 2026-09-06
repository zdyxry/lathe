package normalize

import (
	"errors"
	"fmt"
	"maps"
	"mime"
	"slices"
	"strings"

	"github.com/lathe-cli/lathe/pkg/runtime"
)

var errNestedObjectProperty = errors.New("nested objects are not supported")

var reservedBodyFlagNames = map[string]bool{
	"all":       true,
	"debug":     true,
	"dry-run":   true,
	"file":      true,
	"help":      true,
	"hostname":  true,
	"max-pages": true,
	"output":    true,
	"set":       true,
	"set-str":   true,
	"stream":    true,
	"version":   true,
	"wait":      true,
}

func ExpandJSONBodyFlags(spec runtime.CommandSpec) ([]runtime.ParamSpec, []string, error) {
	if spec.RequestBody == nil {
		return nil, nil, fmt.Errorf("command has no request body")
	}
	if spec.RequestBody.Template != "" {
		return nil, nil, fmt.Errorf("GraphQL request templates cannot enable body flags")
	}
	mediaType, _, err := mime.ParseMediaType(spec.RequestBody.MediaType)
	if spec.RequestBody.MediaType != "" && err != nil {
		return nil, nil, fmt.Errorf("request body media type: %w", err)
	}
	if mediaType == "" {
		mediaType = spec.RequestBody.MediaType
	}
	if isMultipartMediaType(mediaType) || hasFormDataParams(spec.Params) {
		return nil, nil, fmt.Errorf("multipart request bodies cannot enable body flags")
	}
	if !runtimeJSONBodyMediaType(mediaType) {
		return nil, nil, fmt.Errorf("request body media type %q cannot enable body flags", spec.RequestBody.MediaType)
	}
	schema := spec.RequestBody.Schema
	if schema == nil {
		return nil, nil, fmt.Errorf("request body schema is required")
	}
	if err := rejectUnsupportedJSONBodyRootSchema(schema); err != nil {
		return nil, nil, err
	}
	existing := map[string]bool{}
	for _, param := range spec.Params {
		existing[param.Flag] = true
		existing[param.Name] = true
		for _, alias := range param.Aliases {
			existing[alias] = true
		}
	}
	candidates := make([]jsonBodyFlagCandidate, 0)
	setOnly := make([]string, 0)
	if err := collectJSONBodyFlagCandidates(&candidates, &setOnly, schema, "", true); err != nil {
		return nil, nil, err
	}
	out := make([]runtime.ParamSpec, 0, len(candidates))
	seenFlags := map[string]string{}
	for _, candidate := range candidates {
		goType, err := jsonBodyFlagGoType(candidate.schema)
		if err != nil {
			return nil, nil, fmt.Errorf("property %q: %w", candidate.path, err)
		}
		flag := jsonBodyFlagName(candidate.path)
		if flag == "" {
			return nil, nil, fmt.Errorf("property %q does not produce a flag name", candidate.path)
		}
		if reservedBodyFlagNames[flag] {
			return nil, nil, fmt.Errorf("property %q flag %q conflicts with a reserved flag", candidate.path, flag)
		}
		if existing[flag] || existing[candidate.path] {
			return nil, nil, fmt.Errorf("property %q flag %q conflicts with an existing parameter", candidate.path, flag)
		}
		if prior, ok := seenFlags[flag]; ok {
			return nil, nil, fmt.Errorf("properties %q and %q produce the same flag %q", prior, candidate.path, flag)
		}
		seenFlags[flag] = candidate.path
		out = append(out, runtime.ParamSpec{
			Name:     candidate.path,
			Flag:     flag,
			In:       runtime.InBody,
			GoType:   goType,
			Help:     jsonBodyFlagHelp(candidate.path, candidate.schema, candidate.required),
			Required: candidate.required,
			Enum:     append([]string(nil), candidate.schema.Enum...),
			ItemEnum: jsonBodyItemEnum(candidate.schema),
			Format:   candidate.schema.Format,
		})
	}
	if len(out) == 0 {
		return nil, nil, fmt.Errorf("no body properties support typed flags (set-only fields: %s); use --set, --set-str, or --file", strings.Join(setOnly, ", "))
	}
	if len(setOnly) == 0 {
		setOnly = nil
	}
	return out, setOnly, nil
}

func ValidateJSONBodyFlagParams(spec runtime.CommandSpec) error {
	hasBodyFlags := false
	for _, param := range spec.Params {
		if param.In == runtime.InBody {
			hasBodyFlags = true
			break
		}
	}
	if !hasBodyFlags {
		return nil
	}
	if err := runtime.ValidateParamFlags(spec.Params); err != nil {
		return err
	}
	for _, param := range spec.Params {
		names := append([]string{param.Flag}, param.Aliases...)
		for _, name := range names {
			if name == "" {
				continue
			}
			if param.In == runtime.InBody && reservedBodyFlagNames[name] {
				return fmt.Errorf("body property %q flag %q conflicts with a reserved flag", param.Name, name)
			}
		}
	}
	return nil
}

type jsonBodyFlagCandidate struct {
	path     string
	schema   *runtime.SchemaSpec
	required bool
}

func collectJSONBodyFlagCandidates(out *[]jsonBodyFlagCandidate, setOnly *[]string, schema *runtime.SchemaSpec, prefix string, parentRequired bool) error {
	schema = jsonBodyFlagSchema(schema)
	if schema == nil {
		return nil
	}
	if schema.Ref != "" && schema.Type == "" && len(schema.Properties) == 0 && schema.Items == nil {
		if prefix == "" {
			return fmt.Errorf("unresolved schema refs are not supported")
		}
		*setOnly = append(*setOnly, prefix)
		return nil
	}
	if len(schema.AnyOf) > 0 || len(schema.OneOf) > 0 || len(schema.AllOf) > 0 {
		if prefix == "" {
			return fmt.Errorf("oneOf/anyOf/allOf is not supported")
		}
		*setOnly = append(*setOnly, prefix)
		return nil
	}
	if schema.AdditionalProperties != nil && (schema.AdditionalProperties.Allowed || schema.AdditionalProperties.Schema != nil) {
		if prefix == "" {
			return fmt.Errorf("maps are not supported")
		}
		*setOnly = append(*setOnly, prefix)
		return nil
	}
	if schema.Type == "object" || len(schema.Properties) > 0 {
		if len(schema.Properties) == 0 {
			if prefix != "" {
				*setOnly = append(*setOnly, prefix)
			}
			return nil
		}
		required := make(map[string]bool, len(schema.Required))
		for _, name := range schema.Required {
			required[name] = true
		}
		for _, name := range slices.Sorted(maps.Keys(schema.Properties)) {
			path := joinBodyFlagPath(prefix, name)
			if err := collectJSONBodyFlagCandidates(out, setOnly, schema.Properties[name], path, parentRequired && required[name]); err != nil {
				return err
			}
		}
		return nil
	}
	goType, err := jsonBodyFlagGoType(schema)
	if err != nil {
		if prefix == "" {
			return err
		}
		*setOnly = append(*setOnly, prefix)
		return nil
	}
	if goType == "" {
		return nil
	}
	*out = append(*out, jsonBodyFlagCandidate{path: prefix, schema: schema, required: parentRequired})
	return nil
}

func rejectUnsupportedJSONBodyRootSchema(schema *runtime.SchemaSpec) error {
	schema = jsonBodyFlagSchema(schema)
	if schema == nil {
		return fmt.Errorf("schema is required")
	}
	if schema.Ref != "" && schema.Type == "" && len(schema.Properties) == 0 && schema.Items == nil {
		return fmt.Errorf("unresolved schema refs are not supported")
	}
	if len(schema.AnyOf) > 0 || len(schema.OneOf) > 0 || len(schema.AllOf) > 0 {
		return fmt.Errorf("oneOf/anyOf/allOf is not supported")
	}
	if schema.AdditionalProperties != nil && (schema.AdditionalProperties.Allowed || schema.AdditionalProperties.Schema != nil) {
		return fmt.Errorf("maps are not supported")
	}
	if schema.Type != "object" {
		return fmt.Errorf("request body must be a JSON object")
	}
	if len(schema.Properties) == 0 {
		return fmt.Errorf("request body object has no properties")
	}
	return nil
}

func jsonBodyFlagSchema(schema *runtime.SchemaSpec) *runtime.SchemaSpec {
	for schema != nil && schema.Type == "" && len(schema.Properties) == 0 && schema.Items == nil && len(schema.AllOf) == 1 {
		schema = schema.AllOf[0]
	}
	return schema
}

func joinBodyFlagPath(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "." + name
}

func jsonBodyFlagName(path string) string {
	parts := strings.Split(path, ".")
	for i, part := range parts {
		parts[i] = camelToKebab(part)
	}
	return strings.Join(parts, "-")
}

func jsonBodyFlagGoType(schema *runtime.SchemaSpec) (string, error) {
	schema = jsonBodyFlagSchema(schema)
	if schema == nil {
		return "", fmt.Errorf("schema is required")
	}
	if err := rejectUnsupportedJSONBodySchema(schema, false); err != nil {
		return "", err
	}
	switch schema.Type {
	case "string":
		return "string", nil
	case "integer":
		return "int64", nil
	case "number":
		return "float64", nil
	case "boolean":
		return "bool", nil
	case "array":
		if schema.Items == nil {
			return "", fmt.Errorf("array items are required")
		}
		itemType, err := jsonBodyFlagGoType(schema.Items)
		if err != nil {
			return "", err
		}
		if strings.HasPrefix(itemType, "[]") {
			return "", fmt.Errorf("nested arrays are not supported")
		}
		return "[]" + itemType, nil
	default:
		return "", fmt.Errorf("unsupported type %q", schema.Type)
	}
}

func rejectUnsupportedJSONBodySchema(schema *runtime.SchemaSpec, root bool) error {
	schema = jsonBodyFlagSchema(schema)
	if schema == nil {
		return fmt.Errorf("schema is required")
	}
	if schema.Ref != "" && schema.Type == "" && len(schema.Properties) == 0 && schema.Items == nil {
		return fmt.Errorf("unresolved schema refs are not supported")
	}
	if len(schema.AnyOf) > 0 || len(schema.OneOf) > 0 || len(schema.AllOf) > 0 {
		return fmt.Errorf("oneOf/anyOf/allOf is not supported")
	}
	if schema.AdditionalProperties != nil && (schema.AdditionalProperties.Allowed || schema.AdditionalProperties.Schema != nil) {
		return fmt.Errorf("maps are not supported")
	}
	if root {
		if schema.Type != "object" {
			return fmt.Errorf("request body must be a JSON object")
		}
		if len(schema.Properties) == 0 {
			return fmt.Errorf("request body object has no properties")
		}
		return nil
	}
	if schema.Type == "object" || len(schema.Properties) > 0 {
		return errNestedObjectProperty
	}
	return nil
}

func jsonBodyFlagHelp(name string, schema *runtime.SchemaSpec, required bool) string {
	base := name
	if schema != nil && strings.TrimSpace(schema.Description) != "" {
		base = firstLine(schema.Description)
	}
	parts := []string{"body"}
	if required {
		parts = append(parts, "required")
	}
	if schema != nil && schema.Format != "" {
		parts = append(parts, schema.Format)
	}
	if schema != nil && len(schema.Enum) > 0 {
		parts = append(parts, "one of: "+strings.Join(schema.Enum, "|"))
	}
	if items := jsonBodyItemEnum(schema); len(items) > 0 {
		parts = append(parts, "items one of: "+strings.Join(items, "|"))
	}
	return fmt.Sprintf("%s (%s)", base, strings.Join(parts, ", "))
}

func jsonBodyItemEnum(schema *runtime.SchemaSpec) []string {
	if schema == nil || schema.Type != "array" || schema.Items == nil {
		return nil
	}
	return append([]string(nil), schema.Items.Enum...)
}

func runtimeJSONBodyMediaType(mediaType string) bool {
	mt, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(mediaType)), ";")
	mt = strings.TrimSpace(mt)
	return mt == "" || mt == "application/json" || strings.HasSuffix(mt, "+json")
}

func isMultipartMediaType(mediaType string) bool {
	mt, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(mediaType)), ";")
	return strings.TrimSpace(mt) == "multipart/form-data"
}

func hasFormDataParams(params []runtime.ParamSpec) bool {
	for _, param := range params {
		if param.In == runtime.InFormData {
			return true
		}
	}
	return false
}
