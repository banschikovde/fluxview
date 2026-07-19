package flux

import (
	"testing"
)

func TestGetSecretValue(t *testing.T) {
	tests := []struct {
		name     string
		secret   Secret
		key      string
		expected string
	}{
		{
			name: "value from stringData",
			secret: Secret{
				Metadata: ObjectMeta{Name: "test", Namespace: "default"},
				StringData: map[string]string{
					"password": "plain-text-password",
				},
			},
			key:      "password",
			expected: "plain-text-password",
		},
		{
			name: "value from base64-encoded data",
			secret: Secret{
				Metadata: ObjectMeta{Name: "test", Namespace: "default"},
				Data: map[string]string{
					"password": "cGxhaW4tdGV4dC1wYXNzd29yZA==", // base64 of "plain-text-password"
				},
			},
			key:      "password",
			expected: "plain-text-password",
		},
		{
			name: "stringData takes precedence over data",
			secret: Secret{
				Metadata: ObjectMeta{Name: "test", Namespace: "default"},
				StringData: map[string]string{
					"password": "from-string-data",
				},
				Data: map[string]string{
					"password": "ZnJvbS1kYXRh", // base64 of "from-data"
				},
			},
			key:      "password",
			expected: "from-string-data",
		},
		{
			name: "non-existent key",
			secret: Secret{
				Metadata: ObjectMeta{Name: "test", Namespace: "default"},
				Data: map[string]string{
					"other": "value",
				},
			},
			key:      "nonexistent",
			expected: "",
		},
		{
			name: "invalid base64 in data",
			secret: Secret{
				Metadata: ObjectMeta{Name: "test", Namespace: "default"},
				Data: map[string]string{
					"password": "not-valid-base64!!!",
				},
			},
			key:      "password",
			expected: "",
		},
		{
			name: "empty secret",
			secret: Secret{
				Metadata: ObjectMeta{Name: "test", Namespace: "default"},
			},
			key:      "any",
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.secret.GetSecretValue(tt.key)
			if result != tt.expected {
				t.Errorf("GetSecretValue(%q) = %q, want %q", tt.key, result, tt.expected)
			}
		})
	}
}
