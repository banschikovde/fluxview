package yamlutil

import "gopkg.in/yaml.v3"

// NodeNeedsConversion reports whether a parsed document tree holds anything
// the JSON-in-YAML conversion targets: a flow-style (JSON) mapping or
// sequence anywhere, or a nil value under a map key. Documents without
// either keep their original bytes — re-decoding and re-marshaling them
// would alphabetize and reformat every key for no effect.
// Anchors and aliases are left alone — they re-expand on the next parse.
func NodeNeedsConversion(node *yaml.Node) bool {
	if node == nil {
		return false
	}
	switch node.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, child := range node.Content {
			if NodeNeedsConversion(child) {
				return true
			}
		}
	case yaml.MappingNode:
		for i := 1; i < len(node.Content); i += 2 {
			if node.Content[i].Tag == "!!null" {
				return true
			}
			if NodeNeedsConversion(node.Content[i]) {
				return true
			}
		}
	}
	return node.Style&yaml.FlowStyle != 0
}
