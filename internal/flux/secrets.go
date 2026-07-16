package flux

import (
	"bytes"
	"strings"

	"gopkg.in/yaml.v3"
)

// SecretRedactedValue is the placeholder for redacted secret data in output.
const SecretRedactedValue = "*** (SECRET) ***"

// SecretHelmPlaceholder is a YAML-safe placeholder injected into Helm values
// for secret-based valuesFrom. Must not start with YAML special characters
// (like * or &) that break Helm template rendering.
const SecretHelmPlaceholder = "SECRET"

// RedactSecrets scans multi-document YAML and replaces secret data values
// with a visual placeholder. Secret kind resources are preserved but their
// data/stringData fields are replaced with "*** (SECRET) ***".
// Documents are split by text first, so a broken document does not prevent
// processing of subsequent ones.
func RedactSecrets(data []byte) []byte {
	docs := SplitYAMLText(data)
	var buf bytes.Buffer

	for i, doc := range docs {
		if i > 0 {
			buf.WriteString("\n---\n")
		}

		var node yaml.Node
		if err := yaml.Unmarshal([]byte(doc), &node); err != nil {
			// Can't parse — keep original text to avoid data loss.
			buf.WriteString(doc)
			continue
		}

		redactSecretNode(&node)

		var encBuf bytes.Buffer
		enc := yaml.NewEncoder(&encBuf)
		enc.SetIndent(2)
		if err := enc.Encode(&node); err != nil {
			// Encoding failed — keep original text to avoid data loss.
			buf.WriteString(doc)
		} else {
			buf.Write(encBuf.Bytes())
		}
		enc.Close()
	}

	return buf.Bytes()
}

// redactSecretNode checks if the YAML document is a Secret and redacts data fields.
func redactSecretNode(node *yaml.Node) {
	if node.Kind != yaml.DocumentNode {
		return
	}

	// Find the mapping node (the actual YAML content).
	var mapping *yaml.Node
	for _, child := range node.Content {
		if child.Kind == yaml.MappingNode {
			mapping = child
			break
		}
	}
	if mapping == nil {
		return
	}

	// Check kind field.
	kind := getMapValue(mapping, "kind")
	if kind == nil || strings.ToLower(kind.Value) != "secret" {
		return
	}

	// Redact data and stringData fields.
	for _, field := range []string{"data", "stringData"} {
		dataNode := getMapValue(mapping, field)
		if dataNode == nil {
			continue
		}

		redactMappingValues(dataNode)
	}
}

// redactMappingValues replaces all values in a mapping node with the redacted placeholder.
func redactMappingValues(node *yaml.Node) {
	if node.Kind != yaml.MappingNode {
		return
	}

	for i := 1; i < len(node.Content); i += 2 {
		valueNode := node.Content[i]
		valueNode.Value = SecretRedactedValue
		valueNode.Tag = "!!str"
		valueNode.Kind = yaml.ScalarNode
		valueNode.Content = nil
	}
}

// getMapValue gets the value node for a key in a mapping node.
func getMapValue(mapping *yaml.Node, key string) *yaml.Node {
	if mapping.Kind != yaml.MappingNode {
		return nil
	}

	for i := 0; i < len(mapping.Content)-1; i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}
