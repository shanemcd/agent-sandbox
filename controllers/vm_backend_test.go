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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
)

func TestVirtioDiskSerial(t *testing.T) {
	assert.Equal(t, "agentdata", virtioDiskSerial("agent-data"))
	assert.Equal(t, "sandbox", virtioDiskSerial("sandbox"))
	long := strings.Repeat("a", 25)
	assert.Equal(t, strings.Repeat("a", 20), virtioDiskSerial(long))
}

func TestPVCClaimName(t *testing.T) {
	tests := []struct {
		template, sandbox, want string
	}{
		{"sandbox-data", "hermes", "sandbox-data-hermes"},
		{"data", "sb", "data-sb"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			assert.Equal(t, tt.want, pvcClaimName(tt.template, tt.sandbox))
		})
	}
}

func TestCollectVMVolumeMounts(t *testing.T) {
	sandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "hermes"},
		Spec: sandboxv1beta1.SandboxSpec{
			SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
				PodTemplate: sandboxv1beta1.PodTemplate{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{
							Name:  "sandbox",
							Image: "img:latest",
							VolumeMounts: []corev1.VolumeMount{
								{Name: "sandbox-data", MountPath: "/sandbox"},
								{Name: "scratch", MountPath: "/tmp/scratch"}, // not a claim template
								{Name: "sandbox-data", MountPath: "/also"},   // duplicate name ignored
							},
						}},
					},
				},
				VolumeClaimTemplates: []sandboxv1beta1.PersistentVolumeClaimTemplate{
					{EmbeddedObjectMetadata: sandboxv1beta1.EmbeddedObjectMetadata{Name: "sandbox-data"}},
					{EmbeddedObjectMetadata: sandboxv1beta1.EmbeddedObjectMetadata{Name: "unused-claim"}},
				},
			},
		},
	}

	mounts := collectVMVolumeMounts(sandbox)
	require.Len(t, mounts, 1)
	assert.Equal(t, "sandbox-data", mounts[0].Name)
	assert.Equal(t, "/sandbox", mounts[0].MountPath)
	assert.Equal(t, "sandbox-data-hermes", mounts[0].ClaimName)
	assert.Equal(t, "sandboxdata", mounts[0].Serial)
}

func TestAppendVMClaimDisks(t *testing.T) {
	disks := []vmDisk{virtioDisk("containerdisk", "")}
	volumes := []vmVolume{{Name: "containerdisk"}}
	mounts := []vmVolumeMount{{
		Name: "agent-data", MountPath: "/data", ClaimName: "agent-data-sb", Serial: "agentdata",
	}}

	disks, volumes = appendVMClaimDisks(disks, volumes, mounts)
	require.Len(t, disks, 2)
	require.Len(t, volumes, 2)

	assert.Equal(t, "agent-data", disks[1].Name)
	assert.Equal(t, "agentdata", disks[1].Serial)
	assert.Equal(t, "virtio", disks[1].Disk.Bus)

	require.NotNil(t, volumes[1].PersistentVolumeClaim)
	assert.Equal(t, "agent-data", volumes[1].Name)
	assert.Equal(t, "agent-data-sb", volumes[1].PersistentVolumeClaim.ClaimName)

	d2, v2 := appendVMClaimDisks(disks[:1], volumes[:1], nil)
	require.Len(t, d2, 1)
	require.Len(t, v2, 1)
}

func TestBuildVirtualMachineObject(t *testing.T) {
	sandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "hermes", Namespace: "default"},
		Spec: sandboxv1beta1.SandboxSpec{
			SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
				PodTemplate: sandboxv1beta1.PodTemplate{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{
							Name:  "sandbox",
							Image: "hermes:latest",
							VolumeMounts: []corev1.VolumeMount{
								{Name: "sandbox-data", MountPath: "/sandbox"},
							},
						}},
					},
				},
				VolumeClaimTemplates: []sandboxv1beta1.PersistentVolumeClaimTemplate{
					{EmbeddedObjectMetadata: sandboxv1beta1.EmbeddedObjectMetadata{Name: "sandbox-data"}},
				},
			},
		},
	}

	u, err := buildVirtualMachineObject(sandbox, "hermes", "hermes-cloudinit", "hash", collectVMVolumeMounts(sandbox))
	require.NoError(t, err)
	assert.Equal(t, kubevirtVMGVK, u.GroupVersionKind())

	vols, found, err := unstructured.NestedSlice(u.Object, "spec", "template", "spec", "volumes")
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, vols, 3)

	containerDisk := vols[0].(map[string]interface{})
	assert.Equal(t, "hermes:latest", containerDisk["containerDisk"].(map[string]interface{})["image"])

	claimVol := vols[2].(map[string]interface{})
	assert.Equal(t, "sandbox-data-hermes",
		claimVol["persistentVolumeClaim"].(map[string]interface{})["claimName"])

	disks, found, err := unstructured.NestedSlice(u.Object, "spec", "template", "spec", "domain", "devices", "disks")
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, disks, 3)
	claimDisk := disks[2].(map[string]interface{})
	assert.Equal(t, "sandboxdata", claimDisk["serial"])
}
