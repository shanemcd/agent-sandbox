// Copyright 2026 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package kubevirt holds optional RBAC markers for the VirtualMachine runtime
// backend. Controllers that talk to kubevirt.io live in the parent controllers
// package; this package exists so controller-gen can emit a separate ClusterRole
// (agent-sandbox-controller-kubevirt) that Pod-only installs can omit.
package kubevirt

//+kubebuilder:rbac:groups=kubevirt.io,resources=virtualmachines;virtualmachineinstances,verbs=get;list;watch;create;update;patch;delete
// OpenShell VM SA bootstrap: TokenRequest bound to a companion Pod, written
// into a Secret virtio disk for guest IssueSandboxToken / rebootstrap.
// list/watch are required so controller-runtime can cache ServiceAccounts.
//+kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=serviceaccounts/token,verbs=create
