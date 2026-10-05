// Package catalog resolves evaluator references against the manifests that
// workers describe, using the same rules as the Python SDK's registry:
// "namespace/name", an unambiguous bare "name", optionally pinned "@version".
package catalog

import (
	"fmt"
	"sort"
	"strings"

	evalsiv1alpha1 "github.com/abhishek-rnjn/evals.si/gen/go/evalsi/v1alpha1"
)

// Catalog is an immutable index of evaluator manifests.
type Catalog struct {
	byName map[string]*evalsiv1alpha1.EvaluatorManifest
}

// New indexes manifests by name. Later duplicates are ignored.
func New(manifests []*evalsiv1alpha1.EvaluatorManifest) *Catalog {
	c := &Catalog{byName: map[string]*evalsiv1alpha1.EvaluatorManifest{}}
	for _, m := range manifests {
		if _, dup := c.byName[m.GetName()]; !dup {
			c.byName[m.GetName()] = m
		}
	}
	return c
}

// ShortName is the part after the namespace: "builtin/exact-match" -> "exact-match".
func ShortName(name string) string {
	return name[strings.LastIndex(name, "/")+1:]
}

// Resolve finds the manifest a reference names.
func (c *Catalog) Resolve(ref string) (*evalsiv1alpha1.EvaluatorManifest, error) {
	name, version, _ := strings.Cut(ref, "@")
	var found *evalsiv1alpha1.EvaluatorManifest
	if strings.Contains(name, "/") {
		found = c.byName[name]
	} else {
		var matches []string
		for full, m := range c.byName {
			if ShortName(full) == name {
				matches = append(matches, full)
				found = m
			}
		}
		if len(matches) > 1 {
			sort.Strings(matches)
			return nil, fmt.Errorf("%q is ambiguous; use one of: %s", name, strings.Join(matches, ", "))
		}
	}
	if found == nil {
		return nil, fmt.Errorf("unknown evaluator %q", ref)
	}
	if version != "" && version != found.GetVersion() {
		return nil, fmt.Errorf("%q is pinned, but the installed version is %s", ref, found.GetVersion())
	}
	return found, nil
}

// Manifests returns every manifest, sorted by name.
func (c *Catalog) Manifests() []*evalsiv1alpha1.EvaluatorManifest {
	out := make([]*evalsiv1alpha1.EvaluatorManifest, 0, len(c.byName))
	for _, m := range c.byName {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetName() < out[j].GetName() })
	return out
}
