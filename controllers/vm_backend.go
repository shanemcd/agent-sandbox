// Copyright 2025 The Kubernetes Authors.
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

package controllers

import (
	"context"
	"fmt"
	"hash/fnv"
	"strings"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
)

// KubeVirt GroupVersionKinds for unstructured access.
var (
	kubevirtVMGVK = schema.GroupVersionKind{
		Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachine",
	}
	kubevirtVMIGVK = schema.GroupVersionKind{
		Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachineInstance",
	}
)

// vmVolumeMount describes a volumeClaimTemplate referenced by a volumeMount on
// the first container, for attachment as a KubeVirt virtio disk.
type vmVolumeMount struct {
	Name      string // volumeClaimTemplate / volumeMount name
	MountPath string
	ClaimName string // PVC name: <templateName>-<sandboxName>
	Serial    string // virtio disk serial (alphanumeric, <= 20)
}

// pvcClaimName returns the PVC name for a volumeClaimTemplate, matching
// StatefulSet semantics (<templateName>-<sandboxName>).
func pvcClaimName(templateName, sandboxName string) string {
	return templateName + "-" + sandboxName
}

// virtioDiskSerial returns a KubeVirt-compatible disk serial for name.
// Virtio serials must be alphanumeric and at most 20 characters.
func virtioDiskSerial(name string) string {
	var b strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	s := b.String()
	if s == "" {
		h := fnv.New32a()
		_, _ = h.Write([]byte(name))
		s = fmt.Sprintf("%x", h.Sum32())
	}
	if len(s) > 20 {
		s = s[:20]
	}
	return s
}

// collectVMVolumeMounts returns mounts from the first container that reference
// a volumeClaimTemplate. Unreferenced claim templates are omitted (PVC is still
// created by reconcilePVCs, but not attached to the VM).
func collectVMVolumeMounts(sandbox *sandboxv1beta1.Sandbox) []vmVolumeMount {
	claimNames := make(map[string]struct{}, len(sandbox.Spec.VolumeClaimTemplates))
	for _, t := range sandbox.Spec.VolumeClaimTemplates {
		claimNames[t.Name] = struct{}{}
	}
	if len(claimNames) == 0 {
		return nil
	}
	if len(sandbox.Spec.PodTemplate.Spec.Containers) == 0 {
		return nil
	}
	var out []vmVolumeMount
	seen := make(map[string]struct{})
	for _, m := range sandbox.Spec.PodTemplate.Spec.Containers[0].VolumeMounts {
		if _, ok := claimNames[m.Name]; !ok {
			continue
		}
		if _, dup := seen[m.Name]; dup {
			continue
		}
		seen[m.Name] = struct{}{}
		out = append(out, vmVolumeMount{
			Name:      m.Name,
			MountPath: m.MountPath,
			ClaimName: pvcClaimName(m.Name, sandbox.Name),
			Serial:    virtioDiskSerial(m.Name),
		})
	}
	return out
}

// Local typed shapes for the KubeVirt VirtualMachine subset we create.
// Converted to unstructured at the API boundary so we avoid a kubevirt.io dependency.

type vmDisk struct {
	Name   string    `json:"name"`
	Serial string    `json:"serial,omitempty"`
	Disk   vmDiskBus `json:"disk"`
}

type vmDiskBus struct {
	Bus string `json:"bus"`
}

type vmVolume struct {
	Name                  string           `json:"name"`
	ContainerDisk         *vmContainerDisk `json:"containerDisk,omitempty"`
	Secret                *vmSecretVolume  `json:"secret,omitempty"`
	PersistentVolumeClaim *vmPVCVolume     `json:"persistentVolumeClaim,omitempty"`
}

type vmContainerDisk struct {
	Image string `json:"image"`
}

// vmSecretVolume is the KubeVirt Secret volume source (files as virtio disk).
type vmSecretVolume struct {
	SecretName  string `json:"secretName"`
	DefaultMode *int32 `json:"defaultMode,omitempty"`
}

type vmPVCVolume struct {
	ClaimName string `json:"claimName"`
}

type vmInterface struct {
	Name       string         `json:"name"`
	Masquerade map[string]any `json:"masquerade"`
}

type vmNetwork struct {
	Name string         `json:"name"`
	Pod  map[string]any `json:"pod"`
}

type vmDomainCPU struct {
	Cores int64 `json:"cores"`
}

type vmDomainDevices struct {
	Disks        []vmDisk        `json:"disks"`
	Filesystems  []vmFilesystem  `json:"filesystems,omitempty"`
	Interfaces   []vmInterface   `json:"interfaces"`
}

// vmFilesystem is a KubeVirt virtiofs share (Secret/ConfigMap hot-refresh path).
type vmFilesystem struct {
	Name    string         `json:"name"`
	Virtiofs map[string]any `json:"virtiofs"`
}

type vmDomainResources struct {
	Requests map[string]string `json:"requests"`
}

type vmDomain struct {
	CPU       vmDomainCPU       `json:"cpu"`
	Devices   vmDomainDevices   `json:"devices"`
	Resources vmDomainResources `json:"resources"`
}

type vmTemplateSpec struct {
	Domain   vmDomain    `json:"domain"`
	Networks []vmNetwork `json:"networks"`
	Volumes  []vmVolume  `json:"volumes"`
}

type vmTemplate struct {
	Metadata metav1.ObjectMeta `json:"metadata"`
	Spec     vmTemplateSpec    `json:"spec"`
}

type vmSpec struct {
	Running  bool       `json:"running"`
	Template vmTemplate `json:"template"`
}

// kubevirtVirtualMachine is the typed create payload for a KubeVirt VirtualMachine.
type kubevirtVirtualMachine struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Metadata   metav1.ObjectMeta `json:"metadata"`
	Spec       vmSpec            `json:"spec"`
}

func virtioDisk(name, serial string) vmDisk {
	return vmDisk{
		Name:   name,
		Serial: serial,
		Disk:   vmDiskBus{Bus: "virtio"},
	}
}

func pvcVolume(name, claimName string) vmVolume {
	return vmVolume{
		Name: name,
		PersistentVolumeClaim: &vmPVCVolume{
			ClaimName: claimName,
		},
	}
}

func secretVolume(name, secretName string, defaultMode *int32) vmVolume {
	return vmVolume{
		Name: name,
		Secret: &vmSecretVolume{
			SecretName:  secretName,
			DefaultMode: defaultMode,
		},
	}
}

// appendVMClaimDisks appends virtio disks and PVC volumes for the given mounts.
func appendVMClaimDisks(disks []vmDisk, volumes []vmVolume, mounts []vmVolumeMount) ([]vmDisk, []vmVolume) {
	for _, m := range mounts {
		disks = append(disks, virtioDisk(m.Name, m.Serial))
		volumes = append(volumes, pvcVolume(m.Name, m.ClaimName))
	}
	return disks, volumes
}

// appendVMSecretDisks appends virtio ISO disks and Secret volumes for Secret
// mounts that are not delivered via virtiofs (see appendVMSecretFilesystems).
func appendVMSecretDisks(disks []vmDisk, volumes []vmVolume, mounts []vmSecretMount) ([]vmDisk, []vmVolume) {
	for _, m := range mounts {
		if wantsSecretVirtiofs(m) {
			continue
		}
		disks = append(disks, virtioDisk(m.Name, m.Serial))
		volumes = append(volumes, secretVolume(m.Name, m.SecretName, m.DefaultMode))
	}
	return disks, volumes
}

// wantsSecretVirtiofs is true for the OpenShell SA bootstrap token. KubeVirt
// Secret-as-disk ISOs do not propagate updates into a running VMI; virtiofs
// does (same idea as kubelet projected volumes for Pods).
func wantsSecretVirtiofs(m vmSecretMount) bool {
	return m.Name == openshellSATokenVolumeName
}

// appendVMSecretFilesystems appends virtiofs filesystem devices and Secret
// volumes for mounts that need live Secret update propagation.
func appendVMSecretFilesystems(filesystems []vmFilesystem, volumes []vmVolume, mounts []vmSecretMount) ([]vmFilesystem, []vmVolume) {
	for _, m := range mounts {
		if !wantsSecretVirtiofs(m) {
			continue
		}
		filesystems = append(filesystems, vmFilesystem{
			Name:    m.Name,
			Virtiofs: map[string]any{},
		})
		volumes = append(volumes, secretVolume(m.Name, m.SecretName, m.DefaultMode))
	}
	return filesystems, volumes
}

func (r *SandboxReconciler) reconcileVirtualMachine(ctx context.Context, sandbox *sandboxv1beta1.Sandbox, nameHash string) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	ctx, end := r.Tracer.StartSpan(ctx, nil, "reconcileVirtualMachine", nil)
	defer end()

	vmName := sandbox.Name
	result := ctrl.Result{}

	if sandbox.Spec.OperatingMode == sandboxv1beta1.SandboxOperatingModeSuspended {
		return result, r.suspendVirtualMachine(ctx, sandbox, vmName)
	}

	// Mint/refresh the BoundObjectRef SA token Secret before creating the VM
	// so the guest can IssueSandboxToken on first boot and after reboot.
	if wantsOpenshellSABootstrap(sandbox) {
		requeue, err := r.reconcileOpenshellSABootstrap(ctx, sandbox, nameHash)
		if err != nil {
			return ctrl.Result{}, err
		}
		result.RequeueAfter = requeue
	}

	vm := &unstructured.Unstructured{}
	vm.SetGroupVersionKind(kubevirtVMGVK)
	err := r.Get(ctx, types.NamespacedName{Name: vmName, Namespace: sandbox.Namespace}, vm)
	if err != nil {
		if !k8serrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("failed to get VirtualMachine: %w", err)
		}
		logger.Info("Creating VirtualMachine", "VM.Name", vmName)

		pvcMounts := collectVMVolumeMounts(sandbox)
		secretMounts := collectVMSecretMounts(sandbox)
		if err := r.createSandboxMetaSecret(ctx, sandbox, nameHash, pvcMounts, secretMounts); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.createVirtualMachine(ctx, sandbox, vmName, nameHash, pvcMounts, secretMounts); err != nil {
			return ctrl.Result{}, err
		}

		setVMReadyCondition(sandbox, false, sandboxv1beta1.SandboxReasonDependenciesNotReady,
			"VirtualMachine created, waiting for VMI to become Running")
		return result, nil
	}

	running, _, _ := unstructured.NestedBool(vm.Object, "spec", "running")
	if !running {
		logger.Info("Resuming VirtualMachine", "VM.Name", vmName)
		patch := client.MergeFrom(vm.DeepCopy())
		_ = unstructured.SetNestedField(vm.Object, true, "spec", "running")
		if err := r.Patch(ctx, vm, patch); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to patch VM running=true: %w", err)
		}
	}

	return result, r.updateStatusFromVMI(ctx, sandbox, vmName)
}

func (r *SandboxReconciler) createVirtualMachine(ctx context.Context, sandbox *sandboxv1beta1.Sandbox, vmName, nameHash string, pvcMounts []vmVolumeMount, secretMounts []vmSecretMount) error {
	logger := log.FromContext(ctx)

	vm, err := buildVirtualMachineObject(sandbox, vmName, nameHash, pvcMounts, secretMounts)
	if err != nil {
		return err
	}

	if err := ctrl.SetControllerReference(sandbox, vm, r.Scheme); err != nil {
		return fmt.Errorf("SetControllerReference for VirtualMachine: %w", err)
	}
	if err := r.Create(ctx, vm, client.FieldOwner(sandboxControllerFieldOwner)); err != nil {
		if k8serrors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("failed to create VirtualMachine: %w", err)
	}
	logger.Info("Created VirtualMachine", "VM.Name", vmName, "Image", vmContainerImage(sandbox))
	return nil
}

func vmContainerImage(sandbox *sandboxv1beta1.Sandbox) string {
	image := "quay.io/containerdisks/fedora:latest"
	if len(sandbox.Spec.PodTemplate.Spec.Containers) > 0 {
		image = sandbox.Spec.PodTemplate.Spec.Containers[0].Image
	}
	return image
}

func vmLabels(sandbox *sandboxv1beta1.Sandbox, nameHash string) map[string]string {
	labels := map[string]string{sandboxLabel: nameHash}
	for k, v := range sandbox.Spec.PodTemplate.ObjectMeta.Labels {
		if !isSystemLabel(k) {
			labels[k] = v
		}
	}
	return labels
}

// buildVirtualMachineObject constructs the VirtualMachine for create using typed
// local structs, then converts once to unstructured for the dynamic client.
func buildVirtualMachineObject(sandbox *sandboxv1beta1.Sandbox, vmName, nameHash string, pvcMounts []vmVolumeMount, secretMounts []vmSecretMount) (*unstructured.Unstructured, error) {
	image := vmContainerImage(sandbox)
	labels := vmLabels(sandbox, nameHash)

	disks := []vmDisk{
		virtioDisk("containerdisk", ""),
		virtioDisk(sandboxMetaVolumeName, sandboxMetaSerial),
	}
	volumes := []vmVolume{
		{
			Name:          "containerdisk",
			ContainerDisk: &vmContainerDisk{Image: image},
		},
		secretVolume(sandboxMetaVolumeName, sandboxMetaSecretName(sandbox.Name), nil),
	}
	disks, volumes = appendVMClaimDisks(disks, volumes, pvcMounts)
	disks, volumes = appendVMSecretDisks(disks, volumes, secretMounts)
	var filesystems []vmFilesystem
	filesystems, volumes = appendVMSecretFilesystems(filesystems, volumes, secretMounts)

	typed := kubevirtVirtualMachine{
		APIVersion: kubevirtVMGVK.GroupVersion().String(),
		Kind:       kubevirtVMGVK.Kind,
		Metadata: metav1.ObjectMeta{
			Name:      vmName,
			Namespace: sandbox.Namespace,
			Labels:    labels,
		},
		Spec: vmSpec{
			Running: true,
			Template: vmTemplate{
				Metadata: metav1.ObjectMeta{Labels: labels},
				Spec: vmTemplateSpec{
					Domain: vmDomain{
						CPU: vmDomainCPU{Cores: 2},
						Devices: vmDomainDevices{
							Disks:       disks,
							Filesystems: filesystems,
							Interfaces: []vmInterface{{
								Name:       "default",
								Masquerade: map[string]any{},
							}},
						},
						Resources: vmDomainResources{
							Requests: map[string]string{"memory": "2048Mi"},
						},
					},
					Networks: []vmNetwork{{
						Name: "default",
						Pod:  map[string]any{},
					}},
					Volumes: volumes,
				},
			},
		},
	}

	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&typed)
	if err != nil {
		return nil, fmt.Errorf("convert VirtualMachine to unstructured: %w", err)
	}
	u := &unstructured.Unstructured{Object: obj}
	u.SetGroupVersionKind(kubevirtVMGVK)
	return u, nil
}

func setVMReadyCondition(sandbox *sandboxv1beta1.Sandbox, ready bool, reason, message string) {
	status := metav1.ConditionFalse
	if ready {
		status = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&sandbox.Status.Conditions, metav1.Condition{
		Type:               string(sandboxv1beta1.SandboxConditionReady),
		Status:             status,
		ObservedGeneration: sandbox.Generation,
		Reason:             reason,
		Message:            message,
	})
}

func setVMSuspendedConditions(sandbox *sandboxv1beta1.Sandbox) {
	meta.SetStatusCondition(&sandbox.Status.Conditions, metav1.Condition{
		Type:               string(sandboxv1beta1.SandboxConditionSuspended),
		Status:             metav1.ConditionTrue,
		ObservedGeneration: sandbox.Generation,
		Reason:             "VMStopped",
		Message:            "VirtualMachine stopped",
	})
	setVMReadyCondition(sandbox, false, sandboxv1beta1.SandboxReasonSuspended, "Sandbox is suspended")
}

func setVMIPhaseCondition(sandbox *sandboxv1beta1.Sandbox, phase string) {
	switch phase {
	case "Running":
		setVMReadyCondition(sandbox, true, sandboxv1beta1.SandboxReasonDependenciesReady,
			"VirtualMachineInstance is Running")
	case "Succeeded", "Failed":
		reason := "VMI" + phase
		msg := "VirtualMachineInstance " + strings.ToLower(phase)
		setVMReadyCondition(sandbox, false, reason, msg)
		meta.SetStatusCondition(&sandbox.Status.Conditions, metav1.Condition{
			Type:               string(sandboxv1beta1.SandboxConditionFinished),
			Status:             metav1.ConditionTrue,
			ObservedGeneration: sandbox.Generation,
			Reason:             reason,
			Message:            msg,
		})
	default:
		setVMReadyCondition(sandbox, false, sandboxv1beta1.SandboxReasonDependenciesNotReady,
			fmt.Sprintf("VMI phase: %s", phase))
	}
}

func (r *SandboxReconciler) updateStatusFromVMI(ctx context.Context, sandbox *sandboxv1beta1.Sandbox, vmName string) error {
	vmi := &unstructured.Unstructured{}
	vmi.SetGroupVersionKind(kubevirtVMIGVK)

	err := r.Get(ctx, types.NamespacedName{Name: vmName, Namespace: sandbox.Namespace}, vmi)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			sandbox.Status.PodIPs = nil
			sandbox.Status.NodeName = ""
			setVMReadyCondition(sandbox, false, sandboxv1beta1.SandboxReasonDependenciesNotReady,
				"VirtualMachineInstance does not exist yet")
			return nil
		}
		return fmt.Errorf("failed to get VMI: %w", err)
	}

	phase, _, _ := unstructured.NestedString(vmi.Object, "status", "phase")
	nodeName, _, _ := unstructured.NestedString(vmi.Object, "status", "nodeName")

	var podIP string
	if interfaces, found, _ := unstructured.NestedSlice(vmi.Object, "status", "interfaces"); found && len(interfaces) > 0 {
		if iface, ok := interfaces[0].(map[string]interface{}); ok {
			if ip, ok := iface["ipAddress"].(string); ok {
				podIP = ip
			}
		}
	}

	if podIP != "" {
		sandbox.Status.PodIPs = []string{podIP}
	} else {
		sandbox.Status.PodIPs = nil
	}
	sandbox.Status.NodeName = nodeName
	sandbox.Status.LabelSelector = fmt.Sprintf("%s=%s", sandboxLabel, NameHash(sandbox.Name))

	setVMIPhaseCondition(sandbox, phase)
	return nil
}

func (r *SandboxReconciler) suspendVirtualMachine(ctx context.Context, sandbox *sandboxv1beta1.Sandbox, vmName string) error {
	logger := log.FromContext(ctx)

	ctx, end := r.Tracer.StartSpan(ctx, nil, "suspendVirtualMachine", nil)
	defer end()

	vm := &unstructured.Unstructured{}
	vm.SetGroupVersionKind(kubevirtVMGVK)
	err := r.Get(ctx, types.NamespacedName{Name: vmName, Namespace: sandbox.Namespace}, vm)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			sandbox.Status.PodIPs = nil
			sandbox.Status.NodeName = ""
			setVMSuspendedConditions(sandbox)
			return nil
		}
		return fmt.Errorf("failed to get VM for suspend: %w", err)
	}

	running, _, _ := unstructured.NestedBool(vm.Object, "spec", "running")
	if running {
		logger.Info("Suspending VirtualMachine", "VM.Name", vmName)
		patch := client.MergeFrom(vm.DeepCopy())
		_ = unstructured.SetNestedField(vm.Object, false, "spec", "running")
		if err := r.Patch(ctx, vm, patch); err != nil {
			return fmt.Errorf("failed to patch VM running=false: %w", err)
		}
	}

	sandbox.Status.PodIPs = nil
	sandbox.Status.NodeName = ""
	setVMSuspendedConditions(sandbox)
	return nil
}
