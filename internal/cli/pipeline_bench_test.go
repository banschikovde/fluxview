package cli

import (
	"fmt"
	"strings"
	"testing"

	"github.com/banschikovde/fluxview/internal/yamlutil"
)

// benchFixture builds a multi-doc manifest close to a real fleet output:
// workloads, secrets (redaction path), CRDs, CRs, a JSON-in-YAML doc and
// nil-valued fields — enough variety to exercise every pipeline stage.
func benchFixture(docs int) []byte {
	var b strings.Builder
	for i := 0; i < docs; i++ {
		switch i % 10 {
		case 0, 1, 2: // workloads (majority)
			fmt.Fprintf(&b, `apiVersion: apps/v1
kind: Deployment
metadata:
  name: app-%d
  namespace: ns-%d
  creationTimestamp: "2026-01-01T00:00:00Z"
  labels:
    app.kubernetes.io/name: app-%d
    app.kubernetes.io/version: v1.%d.0
  annotations:
    kubernetes.io/change-cause: "rollout %d"
spec:
  replicas: 3
  selector:
    matchLabels:
      app: app-%d
  template:
    metadata:
      labels:
        app: app-%d
    spec:
      serviceAccountName: app-%d
      containers:
        - name: main
          image: registry.example.com/app-%d:v1.%d.2
          ports:
            - containerPort: 8080
          resources:
            limits:
              cpu: 500m
              memory: 512Mi
          env:
            - name: POD_NAME
              valueFrom:
                fieldRef:
                  fieldPath: metadata.name
status:
  observedGeneration: 1
  replicas: 3
  readyReplicas: 3
---
`, i, i%7, i, i%40, i, i, i, i, i, i%40)
		case 3: // secret (redaction path; every other one carries SOPS metadata)
			fmt.Fprintf(&b, `apiVersion: v1
kind: Secret
metadata:
  name: secret-%d
  namespace: ns-%d
type: Opaque
stringData:
  username: user-%d
  password: "s3cret-%d"
`, i, i%7, i, i)
			// Secrets land on i ≡ 3 (mod 10) — all odd; i%20 == 3 picks
			// every second one (3, 23, 43, …), keeping both SOPS-carrying
			// and plain secrets in the fixture.
			if i%20 == 3 {
				fmt.Fprintf(&b, `sops:
  age:
    - recipient: age1exampleexampleexampleexampleexampleexampleexam
      enc: |
        -----BEGIN AGE ENCRYPTED FILE-----
        EXAMPLECIPHERTEXTEXAMPLECIPHERTEXT
        -----END AGE ENCRYPTED FILE-----
  lastmodified: "2026-01-01T00:00:00Z"
  mac: EXAMPLE_MAC_EXAMPLE_MAC_EXAMPLE_MAC
  version: 3.9.4
`)
			}
			b.WriteString("---\n")
		case 4: // operator CR
			fmt.Fprintf(&b, `apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: pg-%d
  namespace: ns-%d
spec:
  instances: 3
  postgresql:
    parameters:
      shared_buffers: 1GB
  storage:
    size: 10Gi
---
`, i, i%7)
		case 5: // CRD (skip path)
			b.WriteString(`apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: widgets.example.com
spec:
  group: example.com
  names:
    kind: Widget
  versions:
    - name: v1
      served: true
---
`)
		case 6: // JSON-in-YAML (convert path)
			fmt.Fprintf(&b, `apiVersion: v1
kind: ConfigMap
metadata: {"name": "cm-%d", "namespace": "ns-%d"}
data:
  key%d: value-%d
---
`, i, i%7, i, i)
		case 7: // nil values (RemoveNilValues path)
			fmt.Fprintf(&b, `apiVersion: v1
kind: Service
metadata:
  name: svc-%d
  namespace: ns-%d
  annotations:
spec:
  ports:
    - port: 80
---
`, i, i%7)
		default: // flux resources
			fmt.Fprintf(&b, `apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: hr-%d
  namespace: ns-%d
spec:
  interval: 10m
  chart:
    spec:
      chart: podinfo
      version: 6.%d.x
      sourceRef:
        kind: HelmRepository
        name: charts
---
`, i, i%7, i%30)
		}
	}
	return []byte(b.String())
}

func BenchmarkProcessResources(b *testing.B) {
	data := benchFixture(300)
	opts := outputOptions{}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		entries := processResources(data, opts)
		if len(entries) == 0 {
			b.Fatal("no entries")
		}
	}
}

func BenchmarkProcessResourcesAllOptions(b *testing.B) {
	data := benchFixture(300)
	opts := outputOptions{
		namespace:  "ns-3",
		skipCRDs:   true,
		stripAttrs: map[string]bool{"creationTimestamp": true, "status": true},
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		entries := processResources(data, opts)
		if len(entries) == 0 {
			b.Fatal("no entries")
		}
	}
}

func BenchmarkBuildResourceMap(b *testing.B) {
	data := benchFixture(300)
	flags := &DiffFlags{}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m := buildResourceMap(data, flags)
		if len(m) == 0 {
			b.Fatal("empty map")
		}
	}
}

func BenchmarkBuildResourceMapStrip(b *testing.B) {
	data := benchFixture(300)
	flags := &DiffFlags{StripAttrs: "creationTimestamp,status"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m := buildResourceMap(data, flags)
		if len(m) == 0 {
			b.Fatal("empty map")
		}
	}
}

// BenchmarkBuildKSContentTail covers the buildKSContent tail: per-overlay
// reorderYAMLFields plus DedupDocs over the combined output.
func BenchmarkBuildKSContentTail(b *testing.B) {
	ks := benchFixture(150)
	overlays := [][]byte{benchFixture(30), benchFixture(30)}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		output := append([]byte(nil), ks...)
		for _, overlay := range overlays {
			if len(output) > 0 {
				output = append(output, []byte("\n---\n")...)
			}
			output = append(output, reorderYAMLFields(overlay)...)
		}
		out := yamlutil.DedupDocs(output)
		if len(out) == 0 {
			b.Fatal("empty output")
		}
	}
}
