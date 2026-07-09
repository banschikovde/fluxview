package flux

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// SecretRedactedValue is the placeholder for redacted secret data.
const SecretRedactedValue = "*** (SECRET) ***"

// RedactSecrets scans multi-document YAML and replaces secret data values
// with a visual placeholder. Secret kind resources are preserved but their
// data/stringData fields are replaced with "*** (SECRET) ***".
func RedactSecrets(data []byte) []byte {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)

	for {
		var node yaml.Node
		if err := decoder.Decode(&node); err != nil {
			if err != io.EOF {
				fmt.Fprintf(os.Stderr, "Warning: YAML parse error, remaining documents skipped: %v\n", err)
			}
			break
		}

		_ = redactSecretNode(&node)

		if err := encoder.Encode(&node); err != nil {
			break
		}
	}
	encoder.Close()

	return buf.Bytes()
}

// redactSecretNode checks if the YAML document is a Secret and redacts data fields.
// Returns true if the document was redacted.
func redactSecretNode(node *yaml.Node) bool {
	if node.Kind != yaml.DocumentNode {
		return false
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
		return false
	}

	// Check kind field.
	kind := getMapValue(mapping, "kind")
	if kind == nil || strings.ToLower(kind.Value) != "secret" {
		return false
	}

	// Redact data and stringData fields.
	for _, field := range []string{"data", "stringData"} {
		dataNode := getMapValue(mapping, field)
		if dataNode == nil {
			continue
		}

		redactMappingValues(dataNode)
	}

	return true
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

// CountSecrets returns the number of Secret resources in the multi-document YAML.
func CountSecrets(data []byte) int {
	count := 0
	decoder := yaml.NewDecoder(bytes.NewReader(data))

	for {
		var node yaml.Node
		if err := decoder.Decode(&node); err != nil {
			if err != io.EOF {
				fmt.Fprintf(os.Stderr, "Warning: YAML parse error in CountSecrets: %v\n", err)
			}
			break
		}

		if node.Kind != yaml.DocumentNode {
			continue
		}

		var mapping *yaml.Node
		for _, child := range node.Content {
			if child.Kind == yaml.MappingNode {
				mapping = child
				break
			}
		}
		if mapping == nil {
			continue
		}

		kind := getMapValue(mapping, "kind")
		if kind != nil && strings.ToLower(kind.Value) == "secret" {
			count++
		}
	}

	return count
}
