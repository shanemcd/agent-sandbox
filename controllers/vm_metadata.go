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
	"encoding/json"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
)

// Guest metadata is delivered as a KubeVirt Secret volume (virtio serial
// sandboxMetaSerial). The guest image mounts that disk and consumes these keys;
// the controller does not assume cloud-init or any other userdata mechanism.
const (
	sandboxMetaSerial     = "sandboxmeta"
	sandboxMetaVolumeName = "sandbox-meta"
	sandboxMetaEnvKey     = "env"
	sandboxMetaVolumesKey = "volumes.json"

	vmVolumeSourcePVC      = "persistentVolumeClaim"
	vmVolumeSourceSecret   = "secret"
	vmVolumeSourceVirtiofs = "virtiofs"
)

// sandboxVolumeMeta is the machine-readable volume mount contract for guests.
type sandboxVolumeMeta struct {
	Name       string `json:"name"`
	Serial     string `json:"serial"`
	MountPath  string `json:"mountPath"`
	Source     string `json:"source"`
	ClaimName  string `json:"claimName,omitempty"`
	SecretName string `json:"secretName,omitempty"`
}

// vmSecretMount describes a Secret volumeMount on the first container for
// attachment as a KubeVirt secret virtio disk.
type vmSecretMount struct {
	Name       string
	MountPath  string
	SecretName string
	Serial     string
	DefaultMode *int32
}

func sandboxMetaSecretName(sandboxName string) string {
	return sandboxName + "-meta"
}

// collectVMSecretMounts returns first-container volumeMounts that reference a
// Secret volume (not a volumeClaimTemplate).
func collectVMSecretMounts(sandbox *sandboxv1beta1.Sandbox) []vmSecretMount {
	if len(sandbox.Spec.PodTemplate.Spec.Containers) == 0 {
		return nil
	}

	claimNames := make(map[string]struct{}, len(sandbox.Spec.VolumeClaimTemplates))
	for _, t := range sandbox.Spec.VolumeClaimTemplates {
		claimNames[t.Name] = struct{}{}
	}

	volumesByName := make(map[string]corev1.Volume, len(sandbox.Spec.PodTemplate.Spec.Volumes))
	for _, v := range sandbox.Spec.PodTemplate.Spec.Volumes {
		volumesByName[v.Name] = v
	}

	var out []vmSecretMount
	seen := make(map[string]struct{})
	for _, m := range sandbox.Spec.PodTemplate.Spec.Containers[0].VolumeMounts {
		if _, isClaim := claimNames[m.Name]; isClaim {
			continue
		}
		if _, dup := seen[m.Name]; dup {
			continue
		}
		vol, ok := volumesByName[m.Name]
		if !ok || vol.Secret == nil {
			continue
		}
		seen[m.Name] = struct{}{}
		secretName := vol.Secret.SecretName
		if secretName == "" {
			secretName = vol.Name
		}
		out = append(out, vmSecretMount{
			Name:        m.Name,
			MountPath:   m.MountPath,
			SecretName:  secretName,
			Serial:      virtioDiskSerial(m.Name),
			DefaultMode: vol.Secret.DefaultMode,
		})
	}
	return out
}

func (r *SandboxReconciler) createSandboxMetaSecret(ctx context.Context, sandbox *sandboxv1beta1.Sandbox, nameHash string, pvcMounts []vmVolumeMount, secretMounts []vmSecretMount) error {
	logger := log.FromContext(ctx)
	secretName := sandboxMetaSecretName(sandbox.Name)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: sandbox.Namespace,
			Labels:    map[string]string{sandboxLabel: nameHash},
		},
		StringData: map[string]string{
			sandboxMetaEnvKey:     buildSandboxEnvFile(sandbox),
			sandboxMetaVolumesKey: buildSandboxVolumesJSON(pvcMounts, secretMounts),
		},
	}

	if err := ctrl.SetControllerReference(sandbox, secret, r.Scheme); err != nil {
		return fmt.Errorf("SetControllerReference for sandbox meta Secret: %w", err)
	}
	if err := r.Create(ctx, secret, client.FieldOwner(sandboxControllerFieldOwner)); err != nil {
		if k8serrors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("failed to create sandbox meta Secret: %w", err)
	}
	logger.Info("Created sandbox meta Secret", "Secret.Name", secretName)
	return nil
}

// buildSandboxEnvFile dumps all container env vars as KEY=VALUE lines suitable
// for systemd EnvironmentFile and shell sourcing.
func buildSandboxEnvFile(sandbox *sandboxv1beta1.Sandbox) string {
	var lines []string
	for _, c := range sandbox.Spec.PodTemplate.Spec.Containers {
		for _, e := range c.Env {
			lines = append(lines, formatEnvFileLine(e.Name, e.Value))
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

func formatEnvFileLine(key, value string) string {
	// systemd EnvironmentFile treats unquoted values with whitespace as truncated.
	// Quote when needed; escape embedded quotes and backslashes.
	if value == "" || !needsEnvFileQuoting(value) {
		return key + "=" + value
	}
	escaped := strings.ReplaceAll(value, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	return key + `="` + escaped + `"`
}

func needsEnvFileQuoting(value string) bool {
	for _, r := range value {
		if r == ' ' || r == '\t' || r == '\n' || r == '"' || r == '\'' || r == '\\' || r == '#' {
			return true
		}
	}
	return false
}

func buildSandboxVolumesJSON(pvcMounts []vmVolumeMount, secretMounts []vmSecretMount) string {
	metas := make([]sandboxVolumeMeta, 0, len(pvcMounts)+len(secretMounts))
	for _, m := range pvcMounts {
		metas = append(metas, sandboxVolumeMeta{
			Name:      m.Name,
			Serial:    m.Serial,
			MountPath: m.MountPath,
			Source:    vmVolumeSourcePVC,
			ClaimName: m.ClaimName,
		})
	}
	for _, m := range secretMounts {
		source := vmVolumeSourceSecret
		if wantsSecretVirtiofs(m) {
			source = vmVolumeSourceVirtiofs
		}
		metas = append(metas, sandboxVolumeMeta{
			Name:       m.Name,
			Serial:     m.Serial,
			MountPath:  m.MountPath,
			Source:     source,
			SecretName: m.SecretName,
		})
	}
	b, err := json.Marshal(metas)
	if err != nil {
		return "[]"
	}
	return string(b) + "\n"
}
