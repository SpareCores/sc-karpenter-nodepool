/*
This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.
*/

// Command catalog writes the KWOK instance-type catalog of every vendor of
// the committed Spare Cores snapshot (see package catalog).
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/SpareCores/sc-karpenter-nodepool/internal/keeper/keepertest"
	"github.com/SpareCores/sc-karpenter-nodepool/test/kwok/catalog"
)

func main() {
	out := flag.String("out", "", "directory to write <vendor>.json files into")
	snapshot := flag.String("snapshot", keepertest.SnapshotDir(), "snapshot directory")
	providerKeys := flag.Bool("provider-keys", true, "put the provider's own label keys into the offerings")
	flag.Parse()
	if *out == "" {
		fmt.Fprintln(os.Stderr, "catalog: -out is required")
		os.Exit(2)
	}
	var opts []catalog.Option
	if !*providerKeys {
		opts = append(opts, catalog.WithoutProviderKeys())
	}
	cats, err := catalog.WriteAll(*snapshot, *out, opts...)
	if err != nil {
		fmt.Fprintln(os.Stderr, "catalog:", err)
		os.Exit(1)
	}
	for _, v := range catalog.Vendors() {
		c := cats[v.Name]
		fmt.Printf("%-9s %4d types, zones %v, capacity types %v, region %s\n",
			v.Name, len(c.Types), c.Zones, c.CapacityTypes, c.Region)
	}
}
