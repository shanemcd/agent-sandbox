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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
)

func TestWantsOpenshellSABootstrap(t *testing.T) {
	mode := int32(0400)
	withSA := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "hermes"},
		Spec: sandboxv1beta1.SandboxSpec{
			SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
				PodTemplate: sandboxv1beta1.PodTemplate{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{
							Name: "sandbox",
							VolumeMounts: []corev1.VolumeMount{{
								Name:      "openshell-sa-token",
								MountPath: "/var/run/secrets/openshell",
							}},
						}},
						Volumes: []corev1.Volume{{
							Name: "openshell-sa-token",
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName:  "hermes-openshell-sa-token",
									DefaultMode: &mode,
								},
							},
						}},
					},
				},
			},
		},
	}
	assert.True(t, wantsOpenshellSABootstrap(withSA))

	tlsOnly := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "hermes"},
		Spec: sandboxv1beta1.SandboxSpec{
			SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
				PodTemplate: sandboxv1beta1.PodTemplate{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{
							Name: "sandbox",
							VolumeMounts: []corev1.VolumeMount{{
								Name:      "openshell-client-tls",
								MountPath: "/etc/openshell-tls/client",
							}},
						}},
						Volumes: []corev1.Volume{{
							Name: "openshell-client-tls",
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{SecretName: "openshell-client-tls"},
							},
						}},
					},
				},
			},
		},
	}
	assert.False(t, wantsOpenshellSABootstrap(tlsOnly))
}

func TestOpenshellSATokenNames(t *testing.T) {
	assert.Equal(t, "hermes-openshell-sa-token", openshellSATokenSecretName("hermes"))
	assert.Equal(t, "hermes-openshell-bootstrap", openshellBootstrapPodName("hermes"))
}

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

func TestCollectVMVolumeMountsExplicitPVC(t *testing.T) {
	sandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "hermes"},
		Spec: sandboxv1beta1.SandboxSpec{
			SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
				PodTemplate: sandboxv1beta1.PodTemplate{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{
							Name: "sandbox",
							VolumeMounts: []corev1.VolumeMount{
								{Name: "workspace", MountPath: "/sandbox"},
							},
						}},
						Volumes: []corev1.Volume{{
							Name: "workspace",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: "workspace-hermes-20gi",
								},
							},
						}},
					},
				},
			},
		},
	}

	mounts := collectVMVolumeMounts(sandbox)
	require.Len(t, mounts, 1)
	assert.Equal(t, "workspace", mounts[0].Name)
	assert.Equal(t, "/sandbox", mounts[0].MountPath)
	assert.Equal(t, "workspace-hermes-20gi", mounts[0].ClaimName)
	assert.Equal(t, "workspace", mounts[0].Serial)
}

func TestCollectVMVolumeMountsExplicitPVCPrefersOverVCT(t *testing.T) {
	sandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "hermes"},
		Spec: sandboxv1beta1.SandboxSpec{
			SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
				PodTemplate: sandboxv1beta1.PodTemplate{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{
							Name: "sandbox",
							VolumeMounts: []corev1.VolumeMount{
								{Name: "workspace", MountPath: "/sandbox"},
							},
						}},
						Volumes: []corev1.Volume{{
							Name: "workspace",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: "workspace-hermes-20gi",
								},
							},
						}},
					},
				},
				VolumeClaimTemplates: []sandboxv1beta1.PersistentVolumeClaimTemplate{
					{EmbeddedObjectMetadata: sandboxv1beta1.EmbeddedObjectMetadata{Name: "workspace"}},
				},
			},
		},
	}

	mounts := collectVMVolumeMounts(sandbox)
	require.Len(t, mounts, 1)
	assert.Equal(t, "workspace-hermes-20gi", mounts[0].ClaimName,
		"explicit podTemplate PVC claimName must win over VCT-derived workspace-hermes")
}

func TestSetVMPVCClaimNames(t *testing.T) {
	vm := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kubevirt.io/v1",
		"kind":       "VirtualMachine",
		"metadata": map[string]interface{}{
			"name":      "hermes",
			"namespace": "default",
		},
		"spec": map[string]interface{}{
			"template": map[string]interface{}{
				"spec": map[string]interface{}{
					"volumes": []interface{}{
						map[string]interface{}{
							"name": "containerdisk",
							"containerDisk": map[string]interface{}{
								"image": "img:latest",
							},
						},
						map[string]interface{}{
							"name": "workspace",
							"persistentVolumeClaim": map[string]interface{}{
								"claimName": "workspace-hermes",
							},
						},
					},
				},
			},
		},
	}}

	claim, found, err := vmPVCClaimName(vm, "workspace")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "workspace-hermes", claim)

	// No-op when claim already matches
	changed, err := setVMPVCClaimNames(vm, []vmVolumeMount{{
		Name: "workspace", ClaimName: "workspace-hermes",
	}})
	require.NoError(t, err)
	assert.False(t, changed)

	changed, err = setVMPVCClaimNames(vm, []vmVolumeMount{{
		Name: "workspace", ClaimName: "workspace-hermes-20gi",
	}})
	require.NoError(t, err)
	assert.True(t, changed)

	claim, found, err = vmPVCClaimName(vm, "workspace")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "workspace-hermes-20gi", claim)

	// Missing volume is skipped (no error, no change)
	changed, err = setVMPVCClaimNames(vm, []vmVolumeMount{{
		Name: "missing", ClaimName: "other",
	}})
	require.NoError(t, err)
	assert.False(t, changed)
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

func TestAppendVMSecretDisks(t *testing.T) {
	mode := int32(0o400)
	disks := []vmDisk{virtioDisk("containerdisk", "")}
	volumes := []vmVolume{{Name: "containerdisk"}}
	mounts := []vmSecretMount{{
		Name: "openshell-client-tls", MountPath: "/etc/openshell-tls/client",
		SecretName: "openshell-client-tls", Serial: "openshellclienttls", DefaultMode: &mode,
	}}

	disks, volumes = appendVMSecretDisks(disks, volumes, mounts)
	require.Len(t, disks, 2)
	require.Len(t, volumes, 2)
	assert.Equal(t, "openshellclienttls", disks[1].Serial)
	require.NotNil(t, volumes[1].Secret)
	assert.Equal(t, "openshell-client-tls", volumes[1].Secret.SecretName)
	require.NotNil(t, volumes[1].Secret.DefaultMode)
	assert.Equal(t, int32(0o400), *volumes[1].Secret.DefaultMode)
}

func TestAppendVMSecretFilesystemsSkipsISOForSAToken(t *testing.T) {
	mode := int32(0o400)
	disks := []vmDisk{virtioDisk("containerdisk", "")}
	volumes := []vmVolume{{Name: "containerdisk"}}
	mounts := []vmSecretMount{
		{
			Name: openshellSATokenVolumeName, MountPath: "/var/run/secrets/openshell",
			SecretName: "hermes-openshell-sa-token", Serial: "openshellsatoken", DefaultMode: &mode,
		},
		{
			Name: "openshell-client-tls", MountPath: "/etc/openshell-tls/client",
			SecretName: "openshell-client-tls", Serial: "openshellclienttls", DefaultMode: &mode,
		},
	}

	disks, volumes = appendVMSecretDisks(disks, volumes, mounts)
	require.Len(t, disks, 2, "SA token must not be an ISO disk")
	require.Len(t, volumes, 2)
	assert.Equal(t, "openshell-client-tls", volumes[1].Name)

	var filesystems []vmFilesystem
	filesystems, volumes = appendVMSecretFilesystems(filesystems, volumes, mounts)
	require.Len(t, filesystems, 1)
	assert.Equal(t, openshellSATokenVolumeName, filesystems[0].Name)
	require.NotNil(t, filesystems[0].Virtiofs)
	require.Len(t, volumes, 3)
	assert.Equal(t, openshellSATokenVolumeName, volumes[2].Name)
	require.NotNil(t, volumes[2].Secret)
	assert.Equal(t, "hermes-openshell-sa-token", volumes[2].Secret.SecretName)
}

func TestSaTokenStillFreshRequiresMatchingPodUID(t *testing.T) {
	uid := types.UID("live-bootstrap-uid")
	exp := time.Now().Add(50 * time.Minute).UTC().Format(time.RFC3339)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				openshellSATokenExpAnnotation:    exp,
				openshellSATokenPodUIDAnnotation: string(uid),
			},
		},
	}
	requeue, ok := saTokenStillFresh(secret, uid)
	assert.True(t, ok)
	assert.Greater(t, requeue, time.Duration(0))

	_, ok = saTokenStillFresh(secret, types.UID("other-uid"))
	assert.False(t, ok)

	secret.Annotations[openshellSATokenPodUIDAnnotation] = ""
	_, ok = saTokenStillFresh(secret, uid)
	assert.False(t, ok)
}

func TestBuildVirtualMachineObject(t *testing.T) {
	mode := int32(0o400)
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
								{Name: "openshell-client-tls", MountPath: "/etc/openshell-tls/client"},
								{Name: openshellSATokenVolumeName, MountPath: "/var/run/secrets/openshell"},
							},
						}},
						Volumes: []corev1.Volume{
							{
								Name: "openshell-client-tls",
								VolumeSource: corev1.VolumeSource{
									Secret: &corev1.SecretVolumeSource{
										SecretName:  "openshell-client-tls",
										DefaultMode: &mode,
									},
								},
							},
							{
								Name: openshellSATokenVolumeName,
								VolumeSource: corev1.VolumeSource{
									Secret: &corev1.SecretVolumeSource{
										SecretName:  "hermes-openshell-sa-token",
										DefaultMode: &mode,
									},
								},
							},
						},
					},
				},
				VolumeClaimTemplates: []sandboxv1beta1.PersistentVolumeClaimTemplate{
					{EmbeddedObjectMetadata: sandboxv1beta1.EmbeddedObjectMetadata{Name: "sandbox-data"}},
				},
			},
		},
	}

	pvcMounts := collectVMVolumeMounts(sandbox)
	secretMounts := collectVMSecretMounts(sandbox)
	cfg := newVMConfig(sandbox, "hash", pvcMounts, secretMounts)
	u, err := buildVirtualMachineObject(sandbox, "hermes", cfg)
	require.NoError(t, err)
	assert.Equal(t, kubevirtVMGVK, u.GroupVersionKind())

	vols, found, err := unstructured.NestedSlice(u.Object, "spec", "template", "spec", "volumes")
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, vols, 5)

	containerDisk := vols[0].(map[string]interface{})
	assert.Equal(t, "hermes:latest", containerDisk["containerDisk"].(map[string]interface{})["image"])

	metaVol := vols[1].(map[string]interface{})
	assert.Equal(t, "hermes-meta", metaVol["secret"].(map[string]interface{})["secretName"])
	_, hasCloudInit := metaVol["cloudInitNoCloud"]
	assert.False(t, hasCloudInit)

	claimVol := vols[2].(map[string]interface{})
	assert.Equal(t, "sandbox-data-hermes",
		claimVol["persistentVolumeClaim"].(map[string]interface{})["claimName"])

	secretVol := vols[3].(map[string]interface{})
	assert.Equal(t, "openshell-client-tls",
		secretVol["secret"].(map[string]interface{})["secretName"])

	saVol := vols[4].(map[string]interface{})
	assert.Equal(t, openshellSATokenVolumeName, saVol["name"])
	assert.Equal(t, "hermes-openshell-sa-token",
		saVol["secret"].(map[string]interface{})["secretName"])

	disks, found, err := unstructured.NestedSlice(u.Object, "spec", "template", "spec", "domain", "devices", "disks")
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, disks, 4, "SA token must be virtiofs, not a disk")

	metaDisk := disks[1].(map[string]interface{})
	assert.Equal(t, sandboxMetaSerial, metaDisk["serial"])
	assert.Equal(t, sandboxMetaVolumeName, metaDisk["name"])

	claimDisk := disks[2].(map[string]interface{})
	assert.Equal(t, "sandboxdata", claimDisk["serial"])

	secretDisk := disks[3].(map[string]interface{})
	assert.Equal(t, "openshellclienttls", secretDisk["serial"])

	filesystems, found, err := unstructured.NestedSlice(u.Object, "spec", "template", "spec", "domain", "devices", "filesystems")
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, filesystems, 1)
	fs := filesystems[0].(map[string]interface{})
	assert.Equal(t, openshellSATokenVolumeName, fs["name"])
	_, hasVirtiofs := fs["virtiofs"]
	assert.True(t, hasVirtiofs)

	// Default resources: 2 CPU cores, 2048Mi memory, no limits.
	cpuCores, found, err := unstructured.NestedInt64(u.Object, "spec", "template", "spec", "domain", "cpu", "cores")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, int64(2), cpuCores)

	mem, found, err := unstructured.NestedString(u.Object, "spec", "template", "spec", "domain", "resources", "requests", "memory")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "2048Mi", mem)

	_, found, _ = unstructured.NestedMap(u.Object, "spec", "template", "spec", "domain", "resources", "limits")
	assert.False(t, found, "no limits when CR omits them")
}

func TestVMContainerResources(t *testing.T) {
	tests := []struct {
		name         string
		resources    corev1.ResourceRequirements
		wantCores    int64
		wantRequests map[string]string
		wantLimits   map[string]string
	}{
		{
			name:         "defaults when no resources set",
			wantCores:    2,
			wantRequests: map[string]string{"memory": "2048Mi"},
		},
		{
			name: "integer CPU request sets cores",
			resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("4"),
				},
			},
			wantCores:    4,
			wantRequests: map[string]string{"memory": "2048Mi"},
		},
		{
			name: "fractional CPU request uses resource string",
			resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("500m"),
				},
			},
			wantCores:    0,
			wantRequests: map[string]string{"memory": "2048Mi", "cpu": "500m"},
		},
		{
			name: "memory override",
			resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceMemory: resource.MustParse("4Gi"),
				},
			},
			wantCores:    2,
			wantRequests: map[string]string{"memory": "4Gi"},
		},
		{
			name: "both requests and limits",
			resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("2"),
					corev1.ResourceMemory: resource.MustParse("4Gi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("4"),
					corev1.ResourceMemory: resource.MustParse("8Gi"),
				},
			},
			wantCores:    2,
			wantRequests: map[string]string{"memory": "4Gi"},
			wantLimits:   map[string]string{"cpu": "4", "memory": "8Gi"},
		},
		{
			name: "limits only",
			resources: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("8"),
					corev1.ResourceMemory: resource.MustParse("16Gi"),
				},
			},
			wantCores:    2,
			wantRequests: map[string]string{"memory": "2048Mi"},
			wantLimits:   map[string]string{"cpu": "8", "memory": "16Gi"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sandbox := &sandboxv1beta1.Sandbox{
				Spec: sandboxv1beta1.SandboxSpec{
					SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
						PodTemplate: sandboxv1beta1.PodTemplate{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{
									Name:      "sandbox",
									Resources: tt.resources,
								}},
							},
						},
					},
				},
			}
			res := vmContainerResources(sandbox)
			assert.Equal(t, tt.wantCores, res.CPUCores)
			assert.Equal(t, tt.wantRequests, res.Requests)
			if tt.wantLimits == nil {
				assert.Nil(t, res.Limits)
			} else {
				assert.Equal(t, tt.wantLimits, res.Limits)
			}
		})
	}

	t.Run("no containers defaults", func(t *testing.T) {
		sandbox := &sandboxv1beta1.Sandbox{}
		res := vmContainerResources(sandbox)
		assert.Equal(t, int64(2), res.CPUCores)
		assert.Equal(t, map[string]string{"memory": "2048Mi"}, res.Requests)
		assert.Nil(t, res.Limits)
	})
}

func buildTestVM(cores int64, requests, limits map[string]string) *unstructured.Unstructured {
	domain := map[string]interface{}{
		"resources": map[string]interface{}{
			"requests": func() map[string]interface{} {
				m := make(map[string]interface{}, len(requests))
				for k, v := range requests {
					m[k] = v
				}
				return m
			}(),
		},
	}
	if cores > 0 {
		domain["cpu"] = map[string]interface{}{"cores": cores}
	}
	if limits != nil {
		lm := make(map[string]interface{}, len(limits))
		for k, v := range limits {
			lm[k] = v
		}
		domain["resources"].(map[string]interface{})["limits"] = lm
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"spec": map[string]interface{}{
			"template": map[string]interface{}{
				"spec": map[string]interface{}{
					"domain": domain,
				},
			},
		},
	}}
}

func TestSetVMResources(t *testing.T) {
	t.Run("no change returns false", func(t *testing.T) {
		vm := buildTestVM(2, map[string]string{"memory": "2048Mi"}, nil)
		cfg := vmResourceConfig{
			CPUCores: 2,
			Requests: map[string]string{"memory": "2048Mi"},
		}
		changed, err := setVMResources(vm, cfg)
		require.NoError(t, err)
		assert.False(t, changed)
	})

	t.Run("CPU cores changed", func(t *testing.T) {
		vm := buildTestVM(2, map[string]string{"memory": "2048Mi"}, nil)
		cfg := vmResourceConfig{
			CPUCores: 4,
			Requests: map[string]string{"memory": "2048Mi"},
		}
		changed, err := setVMResources(vm, cfg)
		require.NoError(t, err)
		assert.True(t, changed)

		cores, _, _ := unstructured.NestedInt64(vm.Object, "spec", "template", "spec", "domain", "cpu", "cores")
		assert.Equal(t, int64(4), cores)
	})

	t.Run("memory changed", func(t *testing.T) {
		vm := buildTestVM(2, map[string]string{"memory": "2048Mi"}, nil)
		cfg := vmResourceConfig{
			CPUCores: 2,
			Requests: map[string]string{"memory": "4Gi"},
		}
		changed, err := setVMResources(vm, cfg)
		require.NoError(t, err)
		assert.True(t, changed)

		mem, _, _ := unstructured.NestedString(vm.Object, "spec", "template", "spec", "domain", "resources", "requests", "memory")
		assert.Equal(t, "4Gi", mem)
	})

	t.Run("integer to fractional CPU", func(t *testing.T) {
		vm := buildTestVM(2, map[string]string{"memory": "2048Mi"}, nil)
		cfg := vmResourceConfig{
			CPUCores: 0,
			Requests: map[string]string{"memory": "2048Mi", "cpu": "500m"},
		}
		changed, err := setVMResources(vm, cfg)
		require.NoError(t, err)
		assert.True(t, changed)

		_, cpuFound, _ := unstructured.NestedInt64(vm.Object, "spec", "template", "spec", "domain", "cpu", "cores")
		assert.False(t, cpuFound, "domain.cpu should be removed for fractional CPU")

		cpuReq, _, _ := unstructured.NestedString(vm.Object, "spec", "template", "spec", "domain", "resources", "requests", "cpu")
		assert.Equal(t, "500m", cpuReq)
	})

	t.Run("limits added", func(t *testing.T) {
		vm := buildTestVM(2, map[string]string{"memory": "2048Mi"}, nil)
		cfg := vmResourceConfig{
			CPUCores: 2,
			Requests: map[string]string{"memory": "2048Mi"},
			Limits:   map[string]string{"cpu": "4", "memory": "8Gi"},
		}
		changed, err := setVMResources(vm, cfg)
		require.NoError(t, err)
		assert.True(t, changed)

		limits, found, _ := unstructured.NestedStringMap(vm.Object, "spec", "template", "spec", "domain", "resources", "limits")
		require.True(t, found)
		assert.Equal(t, "4", limits["cpu"])
		assert.Equal(t, "8Gi", limits["memory"])
	})

	t.Run("limits removed", func(t *testing.T) {
		vm := buildTestVM(2, map[string]string{"memory": "2048Mi"}, map[string]string{"cpu": "4"})
		cfg := vmResourceConfig{
			CPUCores: 2,
			Requests: map[string]string{"memory": "2048Mi"},
			Limits:   nil,
		}
		changed, err := setVMResources(vm, cfg)
		require.NoError(t, err)
		assert.True(t, changed)

		_, found, _ := unstructured.NestedStringMap(vm.Object, "spec", "template", "spec", "domain", "resources", "limits")
		assert.False(t, found, "limits should be removed")
	})
}

func TestVMResourcesNeedSync(t *testing.T) {
	t.Run("in sync returns false", func(t *testing.T) {
		vm := buildTestVM(2, map[string]string{"memory": "2048Mi"}, nil)
		cfg := vmResourceConfig{CPUCores: 2, Requests: map[string]string{"memory": "2048Mi"}}
		assert.False(t, vmResourcesNeedSync(vm, cfg))
	})

	t.Run("different cores returns true", func(t *testing.T) {
		vm := buildTestVM(2, map[string]string{"memory": "2048Mi"}, nil)
		cfg := vmResourceConfig{CPUCores: 4, Requests: map[string]string{"memory": "2048Mi"}}
		assert.True(t, vmResourcesNeedSync(vm, cfg))
	})

	t.Run("stale limits returns true", func(t *testing.T) {
		vm := buildTestVM(2, map[string]string{"memory": "2048Mi"}, map[string]string{"cpu": "4"})
		cfg := vmResourceConfig{CPUCores: 2, Requests: map[string]string{"memory": "2048Mi"}}
		assert.True(t, vmResourcesNeedSync(vm, cfg))
	})
}

func TestVMContainerDiskImageHelpers(t *testing.T) {
	vm := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kubevirt.io/v1",
		"kind":       "VirtualMachine",
		"metadata": map[string]interface{}{
			"name":      "hermes",
			"namespace": "default",
		},
		"spec": map[string]interface{}{
			"template": map[string]interface{}{
				"spec": map[string]interface{}{
					"volumes": []interface{}{
						map[string]interface{}{
							"name": "containerdisk",
							"containerDisk": map[string]interface{}{
								"image": "old:image",
							},
						},
						map[string]interface{}{
							"name": "workspace",
							"persistentVolumeClaim": map[string]interface{}{
								"claimName": "workspace-hermes",
							},
						},
					},
				},
			},
		},
	}}

	got, err := vmContainerDiskImage(vm)
	require.NoError(t, err)
	assert.Equal(t, "old:image", got)

	// No-op path: set to same value
	require.NoError(t, setVMContainerDiskImage(vm, "old:image"))
	got, err = vmContainerDiskImage(vm)
	require.NoError(t, err)
	assert.Equal(t, "old:image", got)

	// Update
	require.NoError(t, setVMContainerDiskImage(vm, "new:image@sha256:abc"))
	got, err = vmContainerDiskImage(vm)
	require.NoError(t, err)
	assert.Equal(t, "new:image@sha256:abc", got)

	// Other volumes preserved
	vols, found, err := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "volumes")
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, vols, 2)
	assert.Equal(t, "workspace", vols[1].(map[string]interface{})["name"])
}

func TestVMContainerDiskImageMissingVolume(t *testing.T) {
	vm := &unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{"name": "hermes", "namespace": "default"},
		"spec": map[string]interface{}{
			"template": map[string]interface{}{
				"spec": map[string]interface{}{
					"volumes": []interface{}{
						map[string]interface{}{"name": "workspace"},
					},
				},
			},
		},
	}}
	_, err := vmContainerDiskImage(vm)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing volume named containerdisk")

	err = setVMContainerDiskImage(vm, "new:image")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing volume named containerdisk")
}

func TestVMContainerDiskImageNoVolumes(t *testing.T) {
	vm := &unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{"name": "hermes", "namespace": "default"},
		"spec":     map[string]interface{}{},
	}}
	_, err := vmContainerDiskImage(vm)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "has no volumes")
}
