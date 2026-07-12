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
	mounts := []vmVolumeMount{{
		Name: "workspace", MountPath: "/sandbox", ClaimName: "workspace-hermes", Serial: "workspace",
	}}
	raw := buildSandboxVolumesJSON(mounts)
	var metas []sandboxVolumeMeta
	require.NoError(t, json.Unmarshal([]byte(raw), &metas))
	require.Len(t, metas, 1)
	assert.Equal(t, "workspace", metas[0].Name)
	assert.Equal(t, "workspace", metas[0].Serial)
	assert.Equal(t, "/sandbox", metas[0].MountPath)
	assert.Equal(t, "workspace-hermes", metas[0].ClaimName)

	empty := buildSandboxVolumesJSON(nil)
	assert.Equal(t, "[]\n", empty)
}

func TestBuildCloudInitUserdata_GenericMetadataOnly(t *testing.T) {
	sandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb"},
		Spec: sandboxv1beta1.SandboxSpec{
			SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
				PodTemplate: sandboxv1beta1.PodTemplate{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{
							Env: []corev1.EnvVar{
								{Name: "OPENSHELL_ENDPOINT", Value: "http://gw.example:8080"},
								{Name: "OPENSHELL_SANDBOX_COMMAND", Value: "nemoclaw-start-vm"},
								{Name: "OPENSHELL_SANDBOX_TOKEN", Value: "tok"},
							},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "workspace", MountPath: "/sandbox"},
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
	userdata := buildCloudInitUserdata(sandbox, mounts, nil)

	assert.Contains(t, userdata, "path: /etc/sandbox/env")
	assert.Contains(t, userdata, "path: /etc/sandbox/volumes.json")
	assert.Contains(t, userdata, "OPENSHELL_ENDPOINT=http://gw.example:8080")
	assert.Contains(t, userdata, "OPENSHELL_SANDBOX_TOKEN=tok")
	assert.Contains(t, userdata, `"serial":"workspace"`)
	assert.Contains(t, userdata, `"mountPath":"/sandbox"`)

	// Product-specific guest bootstrap must not live in the controller.
	assert.NotContains(t, userdata, "openshell-sandbox.service")
	assert.NotContains(t, userdata, "prepare-writable-roots")
	assert.NotContains(t, userdata, "OPENSHELL_PRESERVE")
	assert.NotContains(t, userdata, "hermes")
	assert.NotContains(t, userdata, "runcmd:")
	assert.NotContains(t, userdata, "users:")
}

func TestBuildCloudInitUserdata_ProjectsSecretFiles(t *testing.T) {
	sandbox := &sandboxv1beta1.Sandbox{
		Spec: sandboxv1beta1.SandboxSpec{
			SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
				PodTemplate: sandboxv1beta1.PodTemplate{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{
							Env: []corev1.EnvVar{
								{Name: "OPENSHELL_TLS_CA", Value: "/etc/openshell-tls/client/ca.crt"},
							},
						}},
					},
				},
			},
		},
	}
	secretFiles := []cloudInitFile{{
		Path:        "/etc/openshell-tls/client/ca.crt",
		Permissions: "0400",
		Content:     "-----BEGIN CERTIFICATE-----\nABC\n-----END CERTIFICATE-----\n",
	}, {
		Path:        "/etc/openshell-tls/client/tls.key",
		Permissions: "0400",
		Content:     "-----BEGIN PRIVATE KEY-----\nXYZ\n-----END PRIVATE KEY-----\n",
	}}

	userdata := buildCloudInitUserdata(sandbox, nil, secretFiles)
	assert.Contains(t, userdata, "path: /etc/openshell-tls/client/ca.crt")
	assert.Contains(t, userdata, `permissions: "0400"`)
	assert.Contains(t, userdata, "-----BEGIN CERTIFICATE-----")
	assert.Contains(t, userdata, "path: /etc/openshell-tls/client/tls.key")
	assert.Contains(t, userdata, "OPENSHELL_TLS_CA=/etc/openshell-tls/client/ca.crt")
	assert.NotContains(t, userdata, "openshell-sandbox.service")
}

func TestSecretVolumeFiles(t *testing.T) {
	mode := int32(0o400)
	sec := &corev1.Secret{
		Data: map[string][]byte{
			"ca.crt":  []byte("CA"),
			"tls.crt": []byte("CERT"),
			"tls.key": []byte("KEY"),
		},
	}
	src := &corev1.SecretVolumeSource{
		SecretName:  "openshell-client-tls",
		DefaultMode: &mode,
		Items: []corev1.KeyToPath{
			{Key: "ca.crt", Path: "ca.crt"},
			{Key: "tls.crt", Path: "tls.crt"},
			{Key: "tls.key", Path: "tls.key"},
		},
	}
	files, err := secretVolumeFiles("/etc/openshell-tls/client", src, sec, "0400")
	require.NoError(t, err)
	require.Len(t, files, 3)
	assert.Equal(t, "/etc/openshell-tls/client/ca.crt", files[0].Path)
	assert.Equal(t, "0400", files[0].Permissions)
	assert.Equal(t, "CA", files[0].Content)
	assert.Equal(t, "KEY", files[2].Content)
}
