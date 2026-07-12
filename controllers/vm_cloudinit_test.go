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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
)

func TestGatewayNamespaceFromEndpoint(t *testing.T) {
	tests := []struct {
		endpoint string
		want     string
	}{
		{"https://openshell.openshell.svc.cluster.local:8080", "openshell"},
		{"http://gw.gateway-ns.svc:8080", "gateway-ns"},
		{"https://shortname:443", "openshell"},
		{"openshell.myns.svc", "myns"},
	}
	for _, tt := range tests {
		t.Run(tt.endpoint, func(t *testing.T) {
			assert.Equal(t, tt.want, gatewayNamespaceFromEndpoint(tt.endpoint))
		})
	}
}

func TestReadSandboxContainerEnv(t *testing.T) {
	sandbox := &sandboxv1beta1.Sandbox{
		Spec: sandboxv1beta1.SandboxSpec{
			SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
				PodTemplate: sandboxv1beta1.PodTemplate{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{
							Env: []corev1.EnvVar{
								{Name: "OPENSHELL_ENDPOINT", Value: "https://openshell.openshell.svc:8080"},
								{Name: "OPENSHELL_SANDBOX_TOKEN", Value: "tok"},
								{Name: "OPENSHELL_SANDBOX_COMMAND", Value: "nemoclaw-start-vm"},
								{Name: "OPENSHELL_SSH_AUTHORIZED_KEY", Value: "ssh-ed25519 AAAA"},
								{Name: "CUSTOM", Value: "x"},
							},
						}},
					},
				},
			},
		},
	}

	env := readSandboxContainerEnv(sandbox)
	assert.Equal(t, "https://openshell.openshell.svc:8080", env.Endpoint)
	assert.Equal(t, "tok", env.SandboxToken)
	assert.True(t, env.HasSandboxCommand)
	assert.Equal(t, []string{"ssh-ed25519 AAAA"}, env.SSHAuthorizedKeys)
	require.Len(t, env.Vars, 5)
}

func TestBuildPrepareWritableRootsScript_PVCMounts(t *testing.T) {
	script := buildPrepareWritableRootsScript([]vmVolumeMount{{
		Name: "sandbox-data", MountPath: "/sandbox", ClaimName: "sandbox-data-hermes", Serial: "sandboxdata",
	}})

	assert.Contains(t, script, "mount_pvc_disk")
	assert.Contains(t, script, `mount_pvc_disk "sandboxdata" "/sandbox"`)
	assert.Contains(t, script, "mkfs.ext4")
	assert.Contains(t, script, `/dev/disk/by-id/virtio-${serial}`)
	assert.Contains(t, script, ".workspace-initialized")
	assert.Contains(t, script, "for dir in /opt/data; do")
	assert.NotContains(t, script, "for dir in /sandbox /opt/data; do")
	assert.Contains(t, script, "chown root:sandbox /sandbox")
}

func TestBuildPrepareWritableRootsScript_DefaultTmpfs(t *testing.T) {
	script := buildPrepareWritableRootsScript(nil)
	assert.NotContains(t, script, "mount_pvc_disk")
	assert.Contains(t, script, "for dir in /sandbox /opt/data; do")
}

func TestBuildCloudInitUserdata_SandboxCommandUsesProcessMode(t *testing.T) {
	sandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "hermes"},
		Spec: sandboxv1beta1.SandboxSpec{
			SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
				PodTemplate: sandboxv1beta1.PodTemplate{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{
							Name:  "sandbox",
							Image: "hermes-sandbox-kubevirt:latest",
							Env: []corev1.EnvVar{
								{Name: "OPENSHELL_ENDPOINT", Value: "http://openshell.openshell.svc:8080"},
								{Name: "OPENSHELL_SANDBOX_TOKEN", Value: "test-token"},
								{Name: "OPENSHELL_SANDBOX_COMMAND", Value: "nemoclaw-start-vm"},
							},
						}},
					},
				},
			},
		},
	}

	userdata := buildCloudInitUserdata(sandbox, nil, nil)

	assert.NotContains(t, userdata, "sandbox-workload")
	assert.NotContains(t, userdata, "--mode=network")
	assert.Contains(t, userdata, "ExecStart=/opt/openshell/bin/openshell-sandbox")
	assert.Contains(t, userdata, "Environment=OPENSHELL_SANDBOX_COMMAND=nemoclaw-start-vm")
	assert.Contains(t, userdata, "Environment=OPENSHELL_PRESERVE_SANDBOX_OWNERSHIP=1")
	assert.Contains(t, userdata, "Environment=OPENSHELL_SANDBOX_TOKEN_FILE=/etc/openshell/auth/sandbox.jwt")
	assert.Contains(t, userdata, "path: /etc/openshell/prepare-writable-roots.sh")
	assert.Contains(t, userdata, "openshell-sandbox-prepare.service")
	assert.Contains(t, userdata, "Requires=openshell-sandbox-prepare.service")
	assert.Contains(t, userdata, "chown root:sandbox /sandbox")
	assert.Contains(t, userdata, "chown root:sandbox /sandbox/.hermes")
	assert.Contains(t, userdata, "chmod 1775 /sandbox/.hermes")
	assert.NotContains(t, userdata, "for f in config.yaml .config-hash SOUL.md")
	assert.Contains(t, userdata, "for f in config.yaml SOUL.md")
	assert.Contains(t, userdata, "for dir in /sandbox /opt/data; do")
	assert.NotContains(t, userdata, "mount_pvc_disk")
	assert.Contains(t, userdata, "kernel.printk")
	assert.Contains(t, userdata, "99-openshell-quiet-console.conf")
}

func TestBuildCloudInitUserdata_VolumeClaimMounts(t *testing.T) {
	sandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "hermes"},
		Spec: sandboxv1beta1.SandboxSpec{
			SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
				PodTemplate: sandboxv1beta1.PodTemplate{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{
							Name:  "sandbox",
							Image: "hermes-sandbox-kubevirt:latest",
							VolumeMounts: []corev1.VolumeMount{
								{Name: "sandbox-data", MountPath: "/sandbox"},
							},
							Env: []corev1.EnvVar{
								{Name: "OPENSHELL_ENDPOINT", Value: "http://openshell.openshell.svc:8080"},
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

	userdata := buildCloudInitUserdata(sandbox, nil, collectVMVolumeMounts(sandbox))

	assert.Contains(t, userdata, `mount_pvc_disk "sandboxdata" "/sandbox"`)
	assert.Contains(t, userdata, "mkfs.ext4")
	assert.Contains(t, userdata, "for dir in /opt/data; do")
	assert.NotContains(t, userdata, "for dir in /sandbox /opt/data; do")
}

func TestBuildCloudInitUserdata_WithoutSandboxCommandOmitsPreserve(t *testing.T) {
	sandbox := &sandboxv1beta1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "plain-vm"},
		Spec: sandboxv1beta1.SandboxSpec{
			SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
				PodTemplate: sandboxv1beta1.PodTemplate{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{
							Name:  "sandbox",
							Image: "fedora:latest",
							Env: []corev1.EnvVar{
								{Name: "OPENSHELL_ENDPOINT", Value: "http://openshell.openshell.svc:8080"},
							},
						}},
					},
				},
			},
		},
	}

	userdata := buildCloudInitUserdata(sandbox, nil, nil)

	assert.NotContains(t, userdata, "OPENSHELL_SANDBOX_COMMAND")
	assert.NotContains(t, userdata, "OPENSHELL_PRESERVE_SANDBOX_OWNERSHIP")
	assert.NotContains(t, userdata, "sandbox-workload")
	assert.Contains(t, userdata, "ExecStart=/opt/openshell/bin/openshell-sandbox")
}
