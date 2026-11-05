package main

import (
	"errors"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// publicRoots are the packages published in the public sidecar repository
// (scripts/export-public.sh); everything they import, tests included, is
// published with them.
var publicRoots = []string{"cmd/sidecar", "test/e2e", "examples/agent", "scripts/record"}

// publicPackages is the closure of publicRoots inside this module. Adding a
// package here publishes it: it must not be control-plane code (api, auth,
// store, billing, web, collector...).
var publicPackages = []string{
	"cmd/sidecar",
	"examples/agent",
	"internal/config",
	"internal/decision",
	"internal/events",
	"internal/ingest",
	"internal/logging",
	"internal/metrics",
	"internal/policy",
	"internal/pricing",
	"internal/providers",
	"internal/proxy",
	"internal/savings",
	"internal/sse",
	"internal/transport",
	"internal/version",
	"pkg/openai",
	"scripts/record",
	"test/e2e",
}

// publicModules are the only third-party modules the public closure may use.
var publicModules = []string{"github.com/oklog/ulid/v2", "gopkg.in/yaml.v3"}

// TestPublicClosure fails when the sidecar, or a test of any package it
// imports, starts importing a package outside publicPackages or a module
// outside publicModules: either would leak control-plane code into the public
// repository or break its build. It runs `go list`, so it needs the go tool.
func TestPublicClosure(t *testing.T) {
	module := strings.TrimSpace(goList(t, "-m"))
	roots := make([]string, 0, len(publicRoots))
	for _, r := range publicRoots {
		roots = append(roots, module+"/"+r)
	}
	const nonStd = "{{if not .Standard}}{{.ImportPath}}{{end}}"
	var inModule []string
	for _, p := range lines(goList(t, append([]string{"-tags", "e2e", "-deps", "-f", nonStd}, roots...)...)) {
		if p == module || strings.HasPrefix(p, module+"/") {
			inModule = append(inModule, p)
		}
	}
	seen := map[string]bool{}
	for _, p := range lines(goList(t, append([]string{"-tags", "e2e", "-deps", "-test", "-f", nonStd}, inModule...)...)) {
		p = basePackage(p)
		if seen[p] {
			continue
		}
		seen[p] = true
		if rel, ok := strings.CutPrefix(p, module+"/"); ok {
			if !slices.Contains(publicPackages, rel) {
				t.Errorf("public closure imports %s, which is not in publicPackages", rel)
			}
			continue
		}
		if !slices.ContainsFunc(publicModules, func(m string) bool { return p == m || strings.HasPrefix(p, m+"/") }) {
			t.Errorf("public closure imports %s, whose module is not in publicModules", p)
		}
	}
	for _, rel := range publicPackages {
		if !seen[module+"/"+rel] {
			t.Errorf("publicPackages lists %s, which the closure no longer imports; drop it", rel)
		}
	}
}

// basePackage turns a go list -test entry ("p [p.test]", "p.test", "p_test")
// into the import path of the package directory.
func basePackage(p string) string {
	p, _, _ = strings.Cut(p, " ")
	p = strings.TrimSuffix(p, ".test")
	return strings.TrimSuffix(p, "_test")
}

func goList(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "go", append([]string{"list"}, args...)...).Output()
	if err != nil {
		var stderr string
		if ee := (*exec.ExitError)(nil); errors.As(err, &ee) {
			stderr = string(ee.Stderr)
		}
		t.Fatalf("go list %v: %v\n%s", args, err, stderr)
	}
	return string(out)
}

// lines splits go list output into its non-empty lines; an entry may contain a
// space ("p [p.test]").
func lines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}
