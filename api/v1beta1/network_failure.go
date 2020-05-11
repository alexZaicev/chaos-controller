// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2020 Datadog, Inc.

package v1beta1

import (
	"strconv"
	"strings"

	chaostypes "github.com/DataDog/chaos-controller/types"
	"k8s.io/apimachinery/pkg/types"
)

// NetworkFailureSpec represents a network failure injection
type NetworkFailureSpec struct {
	// +nullable
	Hosts              []string `json:"hosts,omitempty"`
	Port               int      `json:"port"`
	Probability        int      `json:"probability"`
	Protocol           string   `json:"protocol"`
	AllowEstablishment bool     `json:"allowEstablishment,omitempty"`
}

// GenerateArgs generates injection or cleanup pod arguments for the given spec
func (s *NetworkFailureSpec) GenerateArgs(mode chaostypes.PodMode, uid types.UID, containerID, sink string) []string {
	args := []string{}

	switch mode {
	case chaostypes.PodModeInject:
		args = []string{
			"network-failure",
			"inject",
			"--uid",
			string(uid),
			"--metrics-sink",
			sink,
			"--container-id",
			containerID,
			"--port",
			strconv.Itoa(s.Port),
			"--protocol",
			s.Protocol,
			"--probability",
			strconv.Itoa(s.Probability),
			"--hosts",
		}
		args = append(args, strings.Split(strings.Join(s.Hosts, " --hosts "), " ")...)

		// allow establishment
		if s.AllowEstablishment {
			args = append(args, "--allow-establishment")
		}
	case chaostypes.PodModeClean:
		args = []string{
			"network-failure",
			"clean",
			"--uid",
			string(uid),
			"--metrics-sink",
			sink,
			"--container-id",
			containerID,
		}
	}

	return args
}
