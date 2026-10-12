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

	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// RepoRoot returns the absolute path of the repository root.
func RepoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..")
}

// StartEnv starts an envtest control plane with the generated CRDs installed.
// It needs KUBEBUILDER_ASSETS (set in the dev container). The returned stop
// function must be called when the tests are done.
func StartEnv() (*rest.Config, func() error, error) {
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join(RepoRoot(), "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		return nil, nil, fmt.Errorf("starting envtest (is KUBEBUILDER_ASSETS set?): %w", err)
	}
	return cfg, env.Stop, nil
}
