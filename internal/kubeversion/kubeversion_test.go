package kubeversion

import (
	"strings"
	"testing"
)

// TestFromEnv: one table for the strict env grammar shared by every
// command — valid forms pass through as set, everything else errors with
// the variable named, unset stays "".
func TestFromEnv(t *testing.T) {
	for _, tt := range []struct {
		name    string
		env     string
		want    string
		wantErr bool
	}{
		{name: "unset is empty", env: "", want: ""},
		{name: "padded unset is empty", env: "   ", want: ""},
		{name: "full version", env: "1.35.7", want: "1.35.7"},
		{name: "v-prefix", env: "v1.35.7", want: "v1.35.7"},
		{name: "master is invalid (schema-registry concept)", env: "master", wantErr: true},
		{name: "short form is invalid for everyone", env: "1.35", wantErr: true},
		{name: "bare v is invalid", env: "v", wantErr: true},
		{name: "prerelease is invalid", env: "1.36.0-rc.1", wantErr: true},
		{name: "garbage is invalid", env: "bogus", wantErr: true},
		{name: "latest is invalid", env: "latest", wantErr: true},
	} {
		t.Run(tt.name, runFromEnvCase(tt.env, tt.want, tt.wantErr))
	}
}

// runFromEnvCase builds the assertion for one FromEnv table row: err must
// name the variable, success must return the value as set.
func runFromEnvCase(env, want string, wantErr bool) func(*testing.T) {
	return func(t *testing.T) {
		t.Setenv(EnvVar, env)
		got, err := FromEnv()
		if wantErr {
			if err == nil {
				t.Fatalf("FromEnv() = %q, nil; want an error", got)
			}
			if !strings.Contains(err.Error(), EnvVar) {
				t.Errorf("error %q must name the variable", err)
			}
			return
		}
		if err != nil {
			t.Fatalf("FromEnv() error = %v, want nil", err)
		}
		if got != want {
			t.Errorf("FromEnv() = %q, want %q", got, want)
		}
	}
}
