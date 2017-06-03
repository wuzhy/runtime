// Copyright (c) 2017 Intel Corporation
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package oci

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Sirupsen/logrus"
	vc "github.com/containers/virtcontainers"
	"github.com/kubernetes-incubator/cri-o/pkg/annotations"
	spec "github.com/opencontainers/runtime-spec/specs-go"
)

var (
	// ErrNoLinux is an error for missing Linux sections in the OCI configuration file.
	ErrNoLinux = errors.New("missing Linux section")

	// ConfigPathKey is the annotation key to fetch the OCI configuration file path.
	ConfigPathKey = "com.github.containers.virtcontainers.pkg.oci.config_path"

	// BundlePathKey is the annotation key to fetch the OCI configuration file path.
	BundlePathKey = "com.github.containers.virtcontainers.pkg.oci.bundle_path"

	// ContainerTypeKey is the annotation key to fetch container type.
	ContainerTypeKey = "com.github.containers.virtcontainers.pkg.oci.container_type"

	// CRIContainerTypeKeyList lists all the CRI keys that could define
	// the container type from annotations in the config.json.
	CRIContainerTypeKeyList = []string{annotations.ContainerType}

	// CRISandboxNameKeyList lists all the CRI keys that could define
	// the sandbox name (pod ID) from annotations in the config.json.
	CRISandboxNameKeyList = []string{annotations.SandboxName}

	// PodIDPrefix is the prefix added to the container ID when the caller
	// does not define the container type explicitely.
	PodIDPrefix = "pod_"
)

const (
	// StateCreated represents a container that has been created and is
	// ready to be run.
	StateCreated = "created"

	// StateRunning represents a container that's currently running.
	StateRunning = "running"

	// StateStopped represents a container that has been stopped.
	StateStopped = "stopped"
)

// CompatOCIProcess is a structure inheriting from spec.Process defined
// in runtime-spec/specs-go package. The goal is to be compatible with
// both v1.0.0-rc4 and v1.0.0-rc5 since the latter introduced a change
// about the type of the Capabilities field.
// Refer to: https://github.com/opencontainers/runtime-spec/commit/37391fb
type CompatOCIProcess struct {
	spec.Process
	Capabilities interface{} `json:"capabilities,omitempty" platform:"linux"`
}

// CompatOCISpec is a structure inheriting from spec.Spec defined
// in runtime-spec/specs-go package. It relies on the CompatOCIProcess
// structure declared above, in order to be compatible with both
// v1.0.0-rc4 and v1.0.0-rc5.
// Refer to: https://github.com/opencontainers/runtime-spec/commit/37391fb
type CompatOCISpec struct {
	spec.Spec
	Process *CompatOCIProcess `json:"process,omitempty"`
}

// RuntimeConfig aggregates all runtime specific settings
type RuntimeConfig struct {
	VMConfig vc.Resources

	HypervisorType   vc.HypervisorType
	HypervisorConfig vc.HypervisorConfig

	AgentType   vc.AgentType
	AgentConfig interface{}

	ProxyType   vc.ProxyType
	ProxyConfig interface{}

	ShimType   vc.ShimType
	ShimConfig interface{}

	Console string
}

var ociLog = logrus.FieldLogger(logrus.New())

// SetLogger sets the logger for oci package.
func SetLogger(logger logrus.FieldLogger) {
	ociLog = logger
}

func cmdEnvs(spec CompatOCISpec, envs []vc.EnvVar) []vc.EnvVar {
	for _, env := range spec.Process.Env {
		kv := strings.Split(env, "=")
		if len(kv) < 2 {
			continue
		}

		envs = append(envs,
			vc.EnvVar{
				Var:   kv[0],
				Value: kv[1],
			})
	}

	return envs
}

func newHook(h spec.Hook) vc.Hook {
	timeout := 0
	if h.Timeout != nil {
		timeout = *h.Timeout
	}

	return vc.Hook{
		Path:    h.Path,
		Args:    h.Args,
		Env:     h.Env,
		Timeout: timeout,
	}
}

func containerHooks(spec CompatOCISpec) vc.Hooks {
	ociHooks := spec.Hooks
	if ociHooks == nil {
		return vc.Hooks{}
	}

	var hooks vc.Hooks

	for _, h := range ociHooks.Prestart {
		hooks.PreStartHooks = append(hooks.PreStartHooks, newHook(h))
	}

	for _, h := range ociHooks.Poststart {
		hooks.PostStartHooks = append(hooks.PostStartHooks, newHook(h))
	}

	for _, h := range ociHooks.Poststop {
		hooks.PostStopHooks = append(hooks.PostStopHooks, newHook(h))
	}

	return hooks
}

func networkConfig(ocispec CompatOCISpec) (vc.NetworkConfig, error) {
	linux := ocispec.Linux
	if linux == nil {
		return vc.NetworkConfig{}, ErrNoLinux
	}

	var netConf vc.NetworkConfig

	for _, n := range linux.Namespaces {
		if n.Type != spec.NetworkNamespace {
			continue
		}

		netConf.NumInterfaces = 1
		if n.Path != "" {
			netConf.NetNSPath = n.Path
		}
	}

	return netConf, nil
}

// getConfigPath returns the full config path from the bundle
// path provided.
func getConfigPath(bundlePath string) string {
	return filepath.Join(bundlePath, "config.json")
}

// ParseConfigJSON unmarshals the config.json file.
func ParseConfigJSON(bundlePath string) (CompatOCISpec, error) {
	configPath := getConfigPath(bundlePath)
	ociLog.Debugf("converting %s", configPath)

	configByte, err := ioutil.ReadFile(configPath)
	if err != nil {
		return CompatOCISpec{}, err
	}

	var ocispec CompatOCISpec
	if err := json.Unmarshal(configByte, &ocispec); err != nil {
		return CompatOCISpec{}, err
	}

	return ocispec, nil
}

// GetContainerType determines which type of container matches the annotations
// table provided.
func GetContainerType(annotations map[string]string) (vc.ContainerType, error) {
	if containerType, ok := annotations[ContainerTypeKey]; ok {
		return vc.ContainerType(containerType), nil
	}

	ociLog.Errorf("Annotations[%s] not found, cannot determine the container type",
		ContainerTypeKey)
	return vc.UnknownContainerType, fmt.Errorf("Could not find container type")
}

// ContainerType returns the type of container and if the container type was
// found from CRI servers annotations.
func (spec *CompatOCISpec) ContainerType() (vc.ContainerType, bool, error) {
	for _, key := range CRIContainerTypeKeyList {
		containerType, ok := spec.Annotations[key]
		if !ok {
			continue
		}

		switch containerType {
		case annotations.ContainerTypeSandbox:
			return vc.PodSandbox, true, nil
		case annotations.ContainerTypeContainer:
			return vc.PodContainer, true, nil
		}

		return vc.UnknownContainerType, true, fmt.Errorf("Unknown container type %s", containerType)
	}

	return vc.PodSandbox, false, nil
}

// PodID determines the pod ID related to an OCI configuration. This function
// is expected to be called only when the container type is "PodContainer".
func (spec *CompatOCISpec) PodID() (string, error) {
	for _, key := range CRISandboxNameKeyList {
		podID, ok := spec.Annotations[key]
		if ok {
			return podID, nil
		}
	}

	return "", fmt.Errorf("Could not find pod ID")
}

// PodConfig converts an OCI compatible runtime configuration file
// to a virtcontainers pod configuration structure.
func PodConfig(ocispec CompatOCISpec, runtime RuntimeConfig, bundlePath, cid, console string) (vc.PodConfig, error) {
	containerConfig, podID, err := ContainerConfig(ocispec, bundlePath, cid, console)
	if err != nil {
		return vc.PodConfig{}, err
	}

	configPath := getConfigPath(bundlePath)

	networkConfig, err := networkConfig(ocispec)
	if err != nil {
		return vc.PodConfig{}, err
	}

	podConfig := vc.PodConfig{
		ID: podID,

		Hooks: containerHooks(ocispec),

		VMConfig: runtime.VMConfig,

		HypervisorType:   runtime.HypervisorType,
		HypervisorConfig: runtime.HypervisorConfig,

		AgentType:   runtime.AgentType,
		AgentConfig: runtime.AgentConfig,

		ProxyType:   runtime.ProxyType,
		ProxyConfig: runtime.ProxyConfig,

		ShimType:   runtime.ShimType,
		ShimConfig: runtime.ShimConfig,

		NetworkModel:  vc.CNMNetworkModel,
		NetworkConfig: networkConfig,

		Containers: []vc.ContainerConfig{containerConfig},

		Annotations: map[string]string{
			ConfigPathKey: configPath,
			BundlePathKey: bundlePath,
		},
	}

	return podConfig, nil
}

// ContainerConfig converts an OCI compatible runtime configuration
// file to a virtcontainers container configuration structure.
func ContainerConfig(ocispec CompatOCISpec, bundlePath, cid, console string) (vc.ContainerConfig, string, error) {
	configPath := getConfigPath(bundlePath)

	rootfs := ocispec.Root.Path
	if !filepath.IsAbs(rootfs) {
		rootfs = filepath.Join(bundlePath, ocispec.Root.Path)
	}
	ociLog.Debugf("container rootfs: %s", rootfs)

	cmd := vc.Cmd{
		Args:         ocispec.Process.Args,
		Envs:         cmdEnvs(ocispec, []vc.EnvVar{}),
		WorkDir:      ocispec.Process.Cwd,
		User:         strconv.FormatUint(uint64(ocispec.Process.User.UID), 10),
		PrimaryGroup: strconv.FormatUint(uint64(ocispec.Process.User.GID), 10),
		Interactive:  ocispec.Process.Terminal,
		Console:      console,
	}

	cmd.SupplementaryGroups = []string{}
	for _, gid := range ocispec.Process.User.AdditionalGids {
		cmd.SupplementaryGroups = append(cmd.SupplementaryGroups, strconv.FormatUint(uint64(gid), 10))
	}

	containerConfig := vc.ContainerConfig{
		ID:             cid,
		RootFs:         rootfs,
		ReadonlyRootfs: ocispec.Spec.Root.Readonly,
		Cmd:            cmd,
		Annotations: map[string]string{
			ConfigPathKey: configPath,
			BundlePathKey: bundlePath,
		},
	}

	cType, cTypeAnnotationFound, err := ocispec.ContainerType()
	if err != nil {
		return vc.ContainerConfig{}, "", err
	}

	containerConfig.Annotations[ContainerTypeKey] = string(cType)

	podID := cid
	if cType == vc.PodSandbox && !cTypeAnnotationFound {
		podID = fmt.Sprintf("%s%s", PodIDPrefix, cid)
	}

	return containerConfig, podID, nil
}

// StatusToOCIState translates a virtcontainers container status into an OCI state.
func StatusToOCIState(status vc.ContainerStatus) (spec.State, error) {
	state := spec.State{
		Version:     spec.Version,
		ID:          status.ID,
		Status:      StateToOCIState(status.State),
		Pid:         status.PID,
		Bundle:      status.Annotations[BundlePathKey],
		Annotations: status.Annotations,
	}

	return state, nil
}

// StateToOCIState translates a virtcontainers container state into an OCI one.
func StateToOCIState(state vc.State) string {
	switch state.State {
	case vc.StateReady:
		return StateCreated
	case vc.StateRunning:
		return StateRunning
	case vc.StateStopped:
		return StateStopped
	default:
		return ""
	}
}

// EnvVars converts an OCI process environment variables slice
// into a virtcontainers EnvVar slice.
func EnvVars(envs []string) ([]vc.EnvVar, error) {
	var envVars []vc.EnvVar

	envDelimiter := "="
	expectedEnvLen := 2

	for _, env := range envs {
		envSlice := strings.SplitN(env, envDelimiter, expectedEnvLen)

		if len(envSlice) < expectedEnvLen {
			return []vc.EnvVar{}, fmt.Errorf("Wrong string format: %s, expecting only %v parameters separated with %q",
				env, expectedEnvLen, envDelimiter)
		}

		if envSlice[0] == "" {
			return []vc.EnvVar{}, fmt.Errorf("Environment variable cannot be empty")
		}

		envSlice[1] = strings.Trim(envSlice[1], "' ")

		if envSlice[1] == "" {
			return []vc.EnvVar{}, fmt.Errorf("Environment value cannot be empty")
		}

		envVar := vc.EnvVar{
			Var:   envSlice[0],
			Value: envSlice[1],
		}

		envVars = append(envVars, envVar)
	}

	return envVars, nil
}

// GetOCIConfig returns an OCI spec configuration from the annotation
// stored into the container status.
func GetOCIConfig(status vc.ContainerStatus) (CompatOCISpec, error) {
	ociConfigPath, ok := status.Annotations[ConfigPathKey]
	if !ok {
		return CompatOCISpec{}, fmt.Errorf("Annotation[%s] not found", ConfigPathKey)
	}

	data, err := ioutil.ReadFile(ociConfigPath)
	if err != nil {
		return CompatOCISpec{}, err
	}

	var ociSpec CompatOCISpec
	if err := json.Unmarshal(data, &ociSpec); err != nil {
		return CompatOCISpec{}, err
	}

	return ociSpec, nil
}
