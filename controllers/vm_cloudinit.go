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
	"path"
	"strings"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
)

// Guest metadata paths written via cloud-init NoCloud userdata.
// Product-specific guest bootstrap (systemd units, mounts, TLS layout) belongs
// in the VM image, which consumes these files.
const (
	sandboxEnvPath     = "/etc/sandbox/env"
	sandboxVolumesPath = "/etc/sandbox/volumes.json"
)

// sandboxVolumeMeta is the machine-readable volume mount contract for guests.
type sandboxVolumeMeta struct {
	Name      string `json:"name"`
	Serial    string `json:"serial"`
	MountPath string `json:"mountPath"`
	ClaimName string `json:"claimName"`
}

// cloudInitFile is one write_files entry projected into the guest.
type cloudInitFile struct {
	Path        string
	Permissions string // octal, e.g. "0400"
	Content     string
}

func (r *SandboxReconciler) createCloudInitSecret(ctx context.Context, sandbox *sandboxv1beta1.Sandbox, secretName, nameHash string, mounts []vmVolumeMount) error {
	logger := log.FromContext(ctx)

	secretFiles, err := r.collectSecretCloudInitFiles(ctx, sandbox)
	if err != nil {
		return err
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: sandbox.Namespace,
			Labels:    map[string]string{sandboxLabel: nameHash},
		},
		StringData: map[string]string{
			"userdata": buildCloudInitUserdata(sandbox, mounts, secretFiles),
		},
	}

	if err := ctrl.SetControllerReference(sandbox, secret, r.Scheme); err != nil {
		return fmt.Errorf("SetControllerReference for cloud-init Secret: %w", err)
	}
	if err := r.Create(ctx, secret, client.FieldOwner(sandboxControllerFieldOwner)); err != nil {
		if k8serrors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("failed to create cloud-init Secret: %w", err)
	}
	logger.Info("Created cloud-init Secret", "Secret.Name", secretName)
	return nil
}

// collectSecretCloudInitFiles projects Secret volumeMounts on the first
// container into guest files (same namespace as the Sandbox, same semantics as
// a kubelet secret mount). Other volume types are ignored on the VM path.
func (r *SandboxReconciler) collectSecretCloudInitFiles(ctx context.Context, sandbox *sandboxv1beta1.Sandbox) ([]cloudInitFile, error) {
	if len(sandbox.Spec.PodTemplate.Spec.Containers) == 0 {
		return nil, nil
	}

	volumesByName := make(map[string]corev1.Volume, len(sandbox.Spec.PodTemplate.Spec.Volumes))
	for _, v := range sandbox.Spec.PodTemplate.Spec.Volumes {
		volumesByName[v.Name] = v
	}

	var out []cloudInitFile
	for _, mount := range sandbox.Spec.PodTemplate.Spec.Containers[0].VolumeMounts {
		vol, ok := volumesByName[mount.Name]
		if !ok || vol.Secret == nil {
			continue
		}
		secretName := vol.Secret.SecretName
		if secretName == "" {
			secretName = vol.Name
		}

		sec := &corev1.Secret{}
		if err := r.Get(ctx, types.NamespacedName{Name: secretName, Namespace: sandbox.Namespace}, sec); err != nil {
			return nil, fmt.Errorf("get Secret %q for volume %q: %w", secretName, mount.Name, err)
		}

		mode := "0644"
		if vol.Secret.DefaultMode != nil {
			mode = fmt.Sprintf("%04o", *vol.Secret.DefaultMode&0o7777)
		}

		files, err := secretVolumeFiles(mount.MountPath, vol.Secret, sec, mode)
		if err != nil {
			return nil, fmt.Errorf("project Secret %q for volume %q: %w", secretName, mount.Name, err)
		}
		out = append(out, files...)
	}
	return out, nil
}

// secretVolumeFiles mirrors kubelet secret volume key→path mapping.
func secretVolumeFiles(mountPath string, src *corev1.SecretVolumeSource, sec *corev1.Secret, defaultMode string) ([]cloudInitFile, error) {
	if len(src.Items) == 0 {
		out := make([]cloudInitFile, 0, len(sec.Data))
		for key, data := range sec.Data {
			out = append(out, cloudInitFile{
				Path:        path.Join(mountPath, key),
				Permissions: defaultMode,
				Content:     string(data),
			})
		}
		return out, nil
	}

	out := make([]cloudInitFile, 0, len(src.Items))
	for _, item := range src.Items {
		data, ok := sec.Data[item.Key]
		if !ok {
			if src.Optional != nil && *src.Optional {
				continue
			}
			return nil, fmt.Errorf("key %q not found", item.Key)
		}
		rel := item.Path
		if rel == "" {
			rel = item.Key
		}
		mode := defaultMode
		if item.Mode != nil {
			mode = fmt.Sprintf("%04o", *item.Mode&0o7777)
		}
		out = append(out, cloudInitFile{
			Path:        path.Join(mountPath, rel),
			Permissions: mode,
			Content:     string(data),
		})
	}
	return out, nil
}

// buildCloudInitUserdata writes product-neutral guest metadata:
// container env as KEY=VALUE lines, volumeClaimTemplate mounts as JSON, and
// any Secret volumeMounts as write_files.
func buildCloudInitUserdata(sandbox *sandboxv1beta1.Sandbox, mounts []vmVolumeMount, secretFiles []cloudInitFile) string {
	files := []cloudInitFile{
		{
			Path:        sandboxEnvPath,
			Permissions: "0600",
			Content:     buildSandboxEnvFile(sandbox),
		},
		{
			Path:        sandboxVolumesPath,
			Permissions: "0644",
			Content:     buildSandboxVolumesJSON(mounts),
		},
	}
	files = append(files, secretFiles...)

	var b strings.Builder
	b.WriteString("#cloud-config\nwrite_files:\n")
	for _, f := range files {
		fmt.Fprintf(&b, "  - path: %s\n    permissions: %q\n    content: |\n", f.Path, f.Permissions)
		b.WriteString(indentCloudInitBlock(f.Content))
		b.WriteByte('\n')
	}
	return b.String()
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

func buildSandboxVolumesJSON(mounts []vmVolumeMount) string {
	metas := make([]sandboxVolumeMeta, 0, len(mounts))
	for _, m := range mounts {
		metas = append(metas, sandboxVolumeMeta{
			Name:      m.Name,
			Serial:    m.Serial,
			MountPath: m.MountPath,
			ClaimName: m.ClaimName,
		})
	}
	if metas == nil {
		metas = []sandboxVolumeMeta{}
	}
	b, err := json.Marshal(metas)
	if err != nil {
		// Mounts are plain strings; marshal failure is not expected.
		return "[]"
	}
	return string(b) + "\n"
}

// indentCloudInitBlock indents each line for embedding under write_files content: |.
func indentCloudInitBlock(content string) string {
	if content == "" {
		return "      "
	}
	const ind = "      "
	var sb strings.Builder
	for _, line := range strings.Split(strings.TrimRight(content, "\n"), "\n") {
		sb.WriteString(ind)
		sb.WriteString(line)
		sb.WriteByte('\n')
	}
	return strings.TrimRight(sb.String(), "\n")
}
