// Package kubeversion resolves FLUXVIEW_KUBE_VERSION — the pipeline-wide
// pin for the Kubernetes version fluxview assumes. Every command resolves
// it at run time with the same strict grammar; command-specific flags
// (--kubernetes-version) keep their own, looser grammars and override the
// env per command (flag > env > default).
package kubeversion

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// Default is the Kubernetes version assumed when neither FLUXVIEW_KUBE_VERSION
// nor a command-specific flag is set. It is deliberately BELOW the version of
// the k8s.io/* modules this project builds against: those modules (client-go
// v0.37) are also what the Helm SDK derives its own moving "latest" 1.37
// from — exactly what broke charts pinned with an upper kubeVersion bound
// (`kubeVersion: < 1.37.0-0`, issue #14). When raising this default,
// re-check such charts first.
const Default = "1.36.1"

// EnvVar is the pipeline-wide Kubernetes version pin.
const EnvVar = "FLUXVIEW_KUBE_VERSION"

// versionPattern matches the env grammar: a full X.Y.Z with an optional
// "v" prefix. "master" is handled separately by FromEnv.
var versionPattern = regexp.MustCompile(`^v?\d+\.\d+\.\d+$`)

// FromEnv returns $FLUXVIEW_KUBE_VERSION or "" when unset. The env grammar
// is strict for every consumer: a full X.Y.Z with an optional "v" prefix.
// A short form ("1.35"), a bare "v", "master" or garbage is an error —
// the loose command-side grammars (helm renders "1.35" as 1.35.0 while
// validate's schema registry has no such version, and "master" is a
// schema-registry concept charts cannot render against) would otherwise
// pin the pipeline to values that mean different things per command.
func FromEnv() (string, error) {
	v := strings.TrimSpace(os.Getenv(EnvVar))
	if v == "" {
		return "", nil
	}
	if versionPattern.MatchString(v) {
		return v, nil
	}
	return "", fmt.Errorf("invalid %s %q: want a full version like 1.36.1", EnvVar, v)
}
