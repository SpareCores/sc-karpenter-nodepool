/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Package testutil holds helpers shared by test packages.
package testutil

import (
	"fmt"
	"path/filepath"
	"runtime"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	karpenterapis "sigs.k8s.io/karpenter/pkg/apis"
)

// RepoRoot returns the absolute path of the repository root.
func RepoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..")
}

// KarpenterCRDs returns Karpenter core's NodePool and NodeClaim CRDs as
// embedded in the sigs.k8s.io/karpenter module (the version in go.mod,
// v1.14.1), copied so callers may modify them.
func KarpenterCRDs() []*apiextensionsv1.CustomResourceDefinition {
	var out []*apiextensionsv1.CustomResourceDefinition
	for _, crd := range karpenterapis.CRDs {
		if crd.Spec.Group == karpenterapis.Group && (crd.Spec.Names.Kind == "NodePool" || crd.Spec.Names.Kind == "NodeClaim") {
			out = append(out, crd.DeepCopy())
		}
	}
	return out
}

// EnvOptions add CRDs to the envtest control plane.
type EnvOptions struct {
	// CRDDirs are directories of CRD manifests.
	CRDDirs []string
	// CRDs are CRD objects.
	CRDs []*apiextensionsv1.CustomResourceDefinition
}

// StartEnv starts an envtest control plane with the generated CRDs installed.
// It needs KUBEBUILDER_ASSETS (set in the dev container). The returned stop
// function must be called when the tests are done.
func StartEnv() (*rest.Config, func() error, error) {
	return StartEnvWith(EnvOptions{})
}

// StartEnvWith is StartEnv with additional CRDs.
func StartEnvWith(opts EnvOptions) (*rest.Config, func() error, error) {
	env := &envtest.Environment{
		CRDDirectoryPaths:     append([]string{filepath.Join(RepoRoot(), "config", "crd", "bases")}, opts.CRDDirs...),
		CRDs:                  opts.CRDs,
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		return nil, nil, fmt.Errorf("starting envtest (is KUBEBUILDER_ASSETS set?): %w", err)
	}
	return cfg, env.Stop, nil
}
