package inventory

import (
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"

	jsonpath "k8s.io/client-go/util/jsonpath"
	"sigs.k8s.io/yaml"
)

// VersionField is one version extraction attempt on a custom resource: a
// JSONPath into the CR, optionally followed by a regex that pulls the
// version out of the matched string (e.g. the tag out of an image
// reference). Fields are tried in order; the first non-empty value wins.
type VersionField struct {
	JSONPath string `yaml:"jsonpath"`
	Regex    string `yaml:"regex,omitempty"`
}

// Rule extracts the software name and version from one CR kind.
type Rule struct {
	Group    string         `yaml:"group"`
	Kind     string         `yaml:"kind"`
	Software string         `yaml:"software"`
	Operator string         `yaml:"operator,omitempty"`
	Version  []VersionField `yaml:"version"`
	// Image optionally extracts the main container image reference of the
	// CR (shown in the IMAGES column under --images). Fields are tried in
	// order; a field may compose the reference out of several spec paths,
	// e.g. ".spec.image.repository}:{.spec.image.tag". Absent when the CR
	// carries no single main image (or none at all) — the operator decides
	// at reconcile time.
	Image []VersionField `yaml:"image,omitempty"`
}

// ruleKey identifies a rule by its CR coordinates.
func ruleKey(group, kind string) string { return group + "/" + kind }

// compiledField is a VersionField with its regex pre-compiled.
type compiledField struct {
	path  *jsonpath.JSONPath
	regex *regexp.Regexp
}

// CompiledRule is a Rule with JSONPaths parsed and regexes compiled once at
// rule-set build time.
type CompiledRule struct {
	Rule
	fields      []compiledField
	imageFields []compiledField
}

// Extract returns the version found in the CR object (a parsed YAML
// mapping). The second result is the version source; ok reports whether a
// non-empty version was found.
func (cr *CompiledRule) Extract(obj map[string]interface{}) (version string, ok bool) {
	for _, f := range cr.fields {
		v, found := evalJSONPath(f.path, obj)
		if !found {
			continue
		}
		if f.regex != nil {
			v, found = matchVersion(f.regex, v)
			if !found {
				continue
			}
		}
		if v != "" {
			return v, true
		}
	}
	return "", false
}

// ExtractImage returns the main container image reference of the CR when
// the rule defines "image" fields. Unlike version extraction, a field's
// whole template renders (joined): a field may compose the reference out of
// several spec paths (".spec.image.repository}:{.spec.image.tag"). Fields
// are tried in order; the first rendering a non-empty value wins.
func (cr *CompiledRule) ExtractImage(obj map[string]interface{}) (string, bool) {
	for _, f := range cr.imageFields {
		var buf strings.Builder
		if err := f.path.Execute(&buf, obj); err != nil {
			continue
		}
		v := strings.TrimSpace(buf.String())
		// A half-composed reference (missing repository renders ":tag",
		// missing tag renders "repo:") is garbage — skip it like an empty
		// result and let the next field try.
		if v == "" || strings.HasPrefix(v, ":") || strings.HasSuffix(v, ":") {
			continue
		}
		if f.regex != nil {
			matched, ok := matchVersion(f.regex, v)
			if !ok || matched == "" {
				continue
			}
			v = matched
		}
		return v, true
	}
	return "", false
}

// evalJSONPath evaluates a parsed JSONPath over the object and returns the
// first non-empty scalar result as a string. Missing keys are not errors.
func evalJSONPath(jp *jsonpath.JSONPath, obj map[string]interface{}) (string, bool) {
	results, err := jp.FindResults(obj)
	if err != nil {
		return "", false
	}
	for _, batch := range results {
		for _, r := range batch {
			if s, ok := scalarString(r); ok && s != "" {
				return s, true
			}
		}
	}
	return "", false
}

// scalarString renders a reflect.Value produced by JSONPath evaluation as a
// string. Numbers are common in version fields (Zalando's
// spec.postgresql.version: 16).
func scalarString(r reflect.Value) (string, bool) {
	for r.Kind() == reflect.Interface || r.Kind() == reflect.Pointer {
		if r.IsNil() {
			return "", false
		}
		r = r.Elem()
	}
	switch r.Kind() {
	case reflect.String:
		return r.String(), true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return fmt.Sprintf("%d", r.Int()), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return fmt.Sprintf("%d", r.Uint()), true
	case reflect.Float32, reflect.Float64:
		return trimFloat(r.Float()), true
	case reflect.Bool:
		if r.Bool() {
			return "true", true
		}
		return "false", true
	default:
		return "", false
	}
}

// trimFloat renders a float version without a trailing ".0" (16.0 → "16").
func trimFloat(f float64) string {
	s := fmt.Sprintf("%.10g", f)
	return s
}

// matchVersion applies a compiled extraction regex to the matched string.
// The named group "version" wins; otherwise the whole match is used.
func matchVersion(re *regexp.Regexp, s string) (string, bool) {
	m := re.FindStringSubmatch(s)
	if m == nil {
		return "", false
	}
	for i, name := range re.SubexpNames() {
		if name == "version" && m[i] != "" {
			return m[i], true
		}
	}
	if m[0] != "" {
		return m[0], true
	}
	return "", false
}

// RuleSet holds compiled rules keyed by group/kind. User rules override
// built-ins for the same group/kind.
type RuleSet struct {
	rules map[string]CompiledRule
}

// NewRuleSet compiles rules into a lookup set. Later entries override
// earlier ones for the same group/kind.
func NewRuleSet(rules []Rule) (*RuleSet, error) {
	rs := &RuleSet{rules: make(map[string]CompiledRule, len(rules))}
	if err := rs.add(rules); err != nil {
		return nil, err
	}
	return rs, nil
}

// add compiles and inserts rules, overriding any existing rule for the same
// group/kind.
func (rs *RuleSet) add(rules []Rule) error {
	for i, rule := range rules {
		if rule.Group == "" || rule.Kind == "" {
			return fmt.Errorf("rule %d: group and kind are required", i+1)
		}
		if rule.Software == "" {
			return fmt.Errorf("rule %d (%s): software is required", i+1, ruleKey(rule.Group, rule.Kind))
		}
		if len(rule.Version) == 0 {
			return fmt.Errorf("rule %d (%s): at least one version jsonpath is required", i+1, ruleKey(rule.Group, rule.Kind))
		}
		compiled := CompiledRule{Rule: rule}
		var err error
		compiled.fields, err = compileFields(rule.Version, "version", i+1, ruleKey(rule.Group, rule.Kind))
		if err != nil {
			return err
		}
		compiled.imageFields, err = compileFields(rule.Image, "image", i+1, ruleKey(rule.Group, rule.Kind))
		if err != nil {
			return err
		}
		rs.rules[ruleKey(rule.Group, rule.Kind)] = compiled
	}
	return nil
}

// compileFields parses and compiles one field list of a rule (version or
// image). The field value is a client-go jsonpath template — the kubectl
// syntax: expressions in curly braces with literal text between them, e.g.
// "{.spec.image.repository}:{.spec.image.tag}". For convenience a bare
// path without braces (".spec.image.tag") is wrapped automatically, so
// simple fields stay short; composite fields use the canonical braced form.
func compileFields(fields []VersionField, what string, pos int, key string) ([]compiledField, error) {
	var out []compiledField
	for _, vf := range fields {
		if vf.JSONPath == "" {
			return nil, fmt.Errorf("rule %d (%s): %s jsonpath must not be empty", pos, key, what)
		}
		template := vf.JSONPath
		if !strings.Contains(template, "{") {
			template = "{" + template + "}"
		}
		jp := jsonpath.New("fluxview-inventory").AllowMissingKeys(true)
		if err := jp.Parse(template); err != nil {
			return nil, fmt.Errorf("rule %d (%s): invalid %s jsonpath %q: %w", pos, key, what, vf.JSONPath, err)
		}
		var re *regexp.Regexp
		if vf.Regex != "" {
			var err error
			re, err = regexp.Compile(vf.Regex)
			if err != nil {
				return nil, fmt.Errorf("rule %d (%s): invalid %s regex %q: %w", pos, key, what, vf.Regex, err)
			}
		}
		out = append(out, compiledField{path: jp, regex: re})
	}
	return out, nil
}

// Lookup returns the compiled rule for a CR's group and kind.
func (rs *RuleSet) Lookup(group, kind string) (CompiledRule, bool) {
	r, ok := rs.rules[ruleKey(group, kind)]
	return r, ok
}

// Len reports the number of rules (diagnostics and tests).
func (rs *RuleSet) Len() int { return len(rs.rules) }

// OperatorForGroup returns the operator name configured for the given API
// group ("" when no rule covers the group). When several rules of the group
// name different operators, the lexicographically first wins — a stable
// choice for deterministic output.
func (rs *RuleSet) OperatorForGroup(group string) string {
	var found []string
	for _, r := range rs.rules {
		if r.Group == group && r.Operator != "" {
			found = append(found, r.Operator)
		}
	}
	if len(found) == 0 {
		return ""
	}
	sort.Strings(found)
	return found[0]
}

// ruleFile is the on-disk format of a user rules file.
type ruleFile struct {
	Rules []Rule `yaml:"rules"`
}

// ParseRuleFile parses a user rules file (the --rules flag or
// .fluxview/inventory-rules.yaml). Semantic validation (jsonpath, regex,
// required fields) happens in NewRuleSet.
func ParseRuleFile(data []byte) ([]Rule, error) {
	var file ruleFile
	if err := yaml.UnmarshalStrict(data, &file); err != nil {
		return nil, fmt.Errorf("parsing rules file: %w", err)
	}
	return file.Rules, nil
}

// builtinGroups lists the Kubernetes API groups whose kinds are native
// resources, not operator CRs — they never produce cr components (even with
// --all-crs).
var builtinGroups = map[string]bool{
	"":                             true, // core group ("v1" etc.)
	"apps":                         true,
	"batch":                        true,
	"networking.k8s.io":            true,
	"rbac.authorization.k8s.io":    true,
	"policy":                       true,
	"autoscaling":                  true,
	"storage.k8s.io":               true,
	"apiextensions.k8s.io":         true,
	"admissionregistration.k8s.io": true,
	"coordination.k8s.io":          true,
	"discovery.k8s.io":             true,
	"scheduling.k8s.io":            true,
	"node.k8s.io":                  true,
	"certificates.k8s.io":          true,
	"authentication.k8s.io":        true,
	"authorization.k8s.io":         true,
	"events.k8s.io":                true,
	"flowcontrol.apiserver.k8s.io": true,
	"resource.k8s.io":              true,
	"storagemigration.k8s.io":      true,
	"apiregistration.k8s.io":       true,
	"authentication.flowcontrol.apiserver.k8s.io": true,
}

// fluxGroups lists the Flux API groups: their kinds are Flux resources, not
// operator CRs.
var fluxGroups = map[string]bool{
	"source.toolkit.fluxcd.io":           true,
	"kustomize.toolkit.fluxcd.io":        true,
	"helm.toolkit.fluxcd.io":             true,
	"notification.toolkit.fluxcd.io":     true,
	"image.toolkit.fluxcd.io":            true,
	"image-reflector.toolkit.fluxcd.io":  true,
	"image-automation.toolkit.fluxcd.io": true,
}

// IsCRCandidate reports whether a document with the given apiVersion is a
// custom resource eligible for cr components: a non-empty group that is
// neither a native Kubernetes group nor a Flux group.
func IsCRCandidate(apiVersion string) bool {
	group, _, _ := strings.Cut(apiVersion, "/")
	if group == apiVersion {
		// No slash: core "v1" — native resources.
		return false
	}
	if builtinGroups[group] || fluxGroups[group] {
		return false
	}
	return true
}
