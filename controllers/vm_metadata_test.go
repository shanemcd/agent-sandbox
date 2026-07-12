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
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
)

func TestBuildSandboxEnvFile(t *testing.T) {
	sandbox := &sandboxv1beta1.Sandbox{
		Spec: sandboxv1beta1.SandboxSpec{
			SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
				PodTemplate: sandboxv1beta1.PodTemplate{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{
							Env: []corev1.EnvVar{
								{Name: "FOO", Value: "bar"},
								{Name: "QUOTED", Value: `hello "world"`},
								{Name: "SPACED", Value: "a b"},
							},
						}},
					},
				},
			},
		},
	}

	got := buildSandboxEnvFile(sandbox)
	assert.Contains(t, got, "FOO=bar\n")
	assert.Contains(t, got, `QUOTED="hello \"world\""`)
	assert.Contains(t, got, `SPACED="a b"`)
}

func TestBuildSandboxVolumesJSON(t *testing.T) {
	pvc := []vmVolumeMount{{
		Name: "workspace", MountPath: "/sandbox", ClaimName: "workspace-hermes", Serial: "workspace",
	}}
	secrets := []vmSecretMount{{
		Name: "openshell-client-tls", MountPath: "/etc/openshell-tls/client",
		SecretName: "openshell-client-tls", Serial: "openshellclienttls",
	}}
	raw := buildSandboxVolumesJSON(pvc, secrets)
	var metas []sandboxVolumeMeta
	require.NoError(t, json.Unmarshal([]byte(raw), &metas))
	require.Len(t, metas, 2)
	assert.Equal(t, vmVolumeSourcePVC, metas[0].Source)
	assert.Equal(t, "workspace-hermes", metas[0].ClaimName)
	assert.Equal(t, vmVolumeSourceSecret, metas[1].Source)
	assert.Equal(t, "openshell-client-tls", metas[1].SecretName)
	assert.Equal(t, "openshellclienttls", metas[1].Serial)

	empty := buildSandboxVolumesJSON(nil, nil)
	assert.Equal(t, "[]\n", empty)
}

func TestCollectVMSecretMounts(t *testing.T) {
	mode := int32(0o400)
	sandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "hermes"},
		Spec: sandboxv1beta1.SandboxSpec{
			SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
				PodTemplate: sandboxv1beta1.PodTemplate{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{
							VolumeMounts: []corev1.VolumeMount{
								{Name: "workspace", MountPath: "/sandbox"},
								{Name: "openshell-client-tls", MountPath: "/etc/openshell-tls/client"},
								{Name: "scratch", MountPath: "/tmp/scratch"},
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
								Name: "scratch",
								VolumeSource: corev1.VolumeSource{
									EmptyDir: &corev1.EmptyDirVolumeSource{},
								},
							},
						},
					},
				},
				VolumeClaimTemplates: []sandboxv1beta1.PersistentVolumeClaimTemplate{
					{EmbeddedObjectMetadata: sandboxv1beta1.EmbeddedObjectMetadata{Name: "workspace"}},
				},
			},
		},
	}

	mounts := collectVMSecretMounts(sandbox)
	require.Len(t, mounts, 1)
	assert.Equal(t, "openshell-client-tls", mounts[0].Name)
	assert.Equal(t, "/etc/openshell-tls/client", mounts[0].MountPath)
	assert.Equal(t, "openshell-client-tls", mounts[0].SecretName)
	assert.Equal(t, "openshellclienttls", mounts[0].Serial)
	require.NotNil(t, mounts[0].DefaultMode)
	assert.Equal(t, int32(0o400), *mounts[0].DefaultMode)
}

func TestSandboxMetaSecretName(t *testing.T) {
	assert.Equal(t, "hermes-meta", sandboxMetaSecretName("hermes"))
}
