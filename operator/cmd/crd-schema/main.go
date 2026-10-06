// Command crd-schema types the spec of the EvalRun and OnlineEvalPolicy
// CRDs from the API's protobuf messages (RunSpec, OnlineEvalPolicy), so
// `kubectl explain` documents the spec and the API server checks the types
// of the fields it knows.
// It runs after controller-gen (make operator-gen) and rewrites the CRD
// files in place.
//
// Specs are run-file documents, so the schema accepts what run files do:
// field names in proto (snake_case) and JSON (camelCase) form, enums by
// name or number, durations as "30s", "10m" or a number of seconds, and
// free-form Struct and Value fields. A message reached again inside itself
// is left open (x-kubernetes-preserve-unknown-fields), because structural
// schemas cannot recurse. Unknown fields are preserved rather than pruned,
// so the webhook names them; oneofs are not expressed, and the webhook
// checks them too.
package main

import (
	"fmt"
	"os"
	"sort"

	"google.golang.org/protobuf/reflect/protoreflect"
	"sigs.k8s.io/yaml"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
)

// Schema builds the OpenAPI v3 schema of a message.
func Schema(md protoreflect.MessageDescriptor, skip map[string]bool) map[string]any {
	return message(md, map[protoreflect.FullName]bool{}, skip)
}

func open(description string) map[string]any {
	s := map[string]any{"x-kubernetes-preserve-unknown-fields": true}
	if description != "" {
		s["description"] = description
	}
	return s
}

func message(md protoreflect.MessageDescriptor, seen map[protoreflect.FullName]bool, skip map[string]bool) map[string]any {
	switch md.FullName() {
	case "google.protobuf.Struct":
		return map[string]any{"type": "object", "x-kubernetes-preserve-unknown-fields": true}
	case "google.protobuf.Value", "google.protobuf.ListValue":
		return open("")
	case "google.protobuf.Duration":
		return map[string]any{"x-kubernetes-int-or-string": true, "description": `A duration: "250ms", "30s", "10m", "1.5h", or a number of seconds.`}
	case "google.protobuf.Timestamp":
		return map[string]any{"type": "string", "format": "date-time"}
	}
	if seen[md.FullName()] {
		return open(string(md.FullName()) + " (recursive; checked on admission)")
	}
	seen[md.FullName()] = true
	defer delete(seen, md.FullName())
	props := map[string]any{}
	fields := md.Fields()
	for i := range fields.Len() {
		fd := fields.Get(i)
		if skip[string(fd.Name())] {
			continue
		}
		s := field(fd, seen)
		props[string(fd.Name())] = s
		if fd.JSONName() != string(fd.Name()) {
			props[fd.JSONName()] = s
		}
	}
	// Unknown fields are kept, not pruned, so the admission webhook can
	// reject a misspelled field by name instead of the API server dropping it.
	return map[string]any{"type": "object", "properties": props, "x-kubernetes-preserve-unknown-fields": true}
}

func field(fd protoreflect.FieldDescriptor, seen map[protoreflect.FullName]bool) map[string]any {
	if fd.IsMap() {
		return map[string]any{"type": "object", "additionalProperties": scalar(fd.MapValue(), seen)}
	}
	s := scalar(fd, seen)
	if fd.IsList() {
		return map[string]any{"type": "array", "items": s}
	}
	return s
}

func scalar(fd protoreflect.FieldDescriptor, seen map[protoreflect.FullName]bool) map[string]any {
	switch fd.Kind() {
	case protoreflect.MessageKind, protoreflect.GroupKind:
		return message(fd.Message(), seen, nil)
	case protoreflect.EnumKind:
		values := fd.Enum().Values()
		names := make([]string, 0, values.Len())
		for i := range values.Len() {
			names = append(names, string(values.Get(i).Name()))
		}
		sort.Strings(names)
		return map[string]any{"x-kubernetes-int-or-string": true, "description": fmt.Sprintf("One of %v, or its number.", names)}
	case protoreflect.BoolKind:
		return map[string]any{"type": "boolean"}
	case protoreflect.StringKind:
		return map[string]any{"type": "string"}
	case protoreflect.BytesKind:
		return map[string]any{"type": "string", "format": "byte"}
	case protoreflect.FloatKind, protoreflect.DoubleKind:
		return map[string]any{"type": "number"}
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind,
		protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		// protojson writes 64-bit integers as strings.
		return map[string]any{"x-kubernetes-int-or-string": true}
	default:
		return map[string]any{"type": "integer"}
	}
}

// patch replaces the spec schema of the CRD file at path.
func patch(path string, spec map[string]any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var crd map[string]any
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		return err
	}
	versions := crd["spec"].(map[string]any)["versions"].([]any)
	for _, v := range versions {
		root := v.(map[string]any)["schema"].(map[string]any)["openAPIV3Schema"].(map[string]any)
		props := root["properties"].(map[string]any)
		old := props["spec"].(map[string]any)
		if d, ok := old["description"]; ok {
			spec["description"] = d
		}
		props["spec"] = spec
	}
	out, err := yaml.Marshal(crd)
	if err != nil {
		return err
	}
	return os.WriteFile(path, append([]byte("---\n"), out...), 0o644)
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: crd-schema CRD_DIR")
		os.Exit(2)
	}
	dir := os.Args[1]
	run := Schema((&evalsiv1alpha1.RunSpec{}).ProtoReflect().Descriptor(), nil)
	// name, project and labels come from the resource's metadata.
	policy := Schema((&evalsiv1alpha1.OnlineEvalPolicy{}).ProtoReflect().Descriptor(),
		map[string]bool{"name": true, "project": true, "labels": true})
	for file, spec := range map[string]map[string]any{
		"evals.si_evalruns.yaml":           run,
		"evals.si_onlineevalpolicies.yaml": policy,
	} {
		if err := patch(dir+"/"+file, spec); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", file, err)
			os.Exit(1)
		}
	}
}
