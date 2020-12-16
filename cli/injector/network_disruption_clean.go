// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2020 Datadog, Inc.

package main

import (
	"github.com/DataDog/chaos-controller/api/v1beta1"
	"github.com/DataDog/chaos-controller/injector"
	"github.com/spf13/cobra"
)

var networkDisruptionCleanCmd = &cobra.Command{
	Use:   "clean",
	Short: "Clean injected network failures",
	Run: func(cmd *cobra.Command, args []string) {
		hosts, _ := cmd.Flags().GetStringSlice("hosts")

		// prepare spec
		spec := v1beta1.NetworkDisruptionSpec{
			Hosts: hosts,
		}

		i := injector.NewNetworkDisruptionInjector(spec, injector.NetworkDisruptionInjectorConfig{Config: config})
		i.Clean()
	},
}
