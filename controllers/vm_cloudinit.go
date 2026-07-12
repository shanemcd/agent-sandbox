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

const (
	supervisorBinary = "/opt/openshell/bin/openshell-sandbox"
	sandboxTokenPath = "/etc/openshell/auth/sandbox.jwt"
	clientTLSDir     = "/etc/openshell-tls/client"
	sshPort          = 22

	// cloudInitContentIndent is the indent for write_files content: | blocks.
	cloudInitContentIndent = "      "
)

// Named bash fragments for prepare-writable-roots.sh (without cloud-init indent).
const (
	prepareScriptHeader = `#!/bin/bash
# Runs every boot (via openshell-sandbox-prepare.service). cloud-init
# runcmd only runs on first boot, so bootc/ostree read-only roots must
# be re-overlaid after every reboot before openshell-sandbox starts.
set -euo pipefail
# Keep audit/info printk off VNC/serial; journal still retains them.
sysctl -q -w kernel.printk="3 4 1 7" 2>/dev/null || true
`

	prepareMountPVCDiskFunc = `# Mount volumeClaimTemplate disks (virtio serial -> mountPath).
# First boot seeds from the image tree (same idea as OpenShell's
# workspace-init), using .workspace-initialized as the sentinel.
mount_pvc_disk() {
  local serial="$1" mount_path="$2" device seed
  device="/dev/disk/by-id/virtio-${serial}"
  for _ in $(seq 1 60); do
    [ -e "$device" ] && break
    sleep 1
  done
  if [ ! -e "$device" ]; then
    echo "PVC disk serial=${serial} not found at $device" >&2
    return 1
  fi
  if ! blkid "$device" >/dev/null 2>&1; then
    mkfs.ext4 -F "$device"
  fi
  # Snapshot image contents before the PVC covers mount_path.
  seed=$(mktemp -d)
  if [ -d "$mount_path" ] && ! mountpoint -q "$mount_path" 2>/dev/null; then
    tar -C "$mount_path" -cf - . 2>/dev/null | tar -C "$seed" -xpf - 2>/dev/null || true
  fi
  mkdir -p "$mount_path"
  if ! mountpoint -q "$mount_path" 2>/dev/null; then
    mount "$device" "$mount_path"
  fi
  if [ ! -f "$mount_path/.workspace-initialized" ]; then
    if [ -n "$(ls -A "$seed" 2>/dev/null)" ]; then
      tar -C "$seed" -cf - . | tar -C "$mount_path" -xpf -
    fi
    touch "$mount_path/.workspace-initialized"
  fi
  rm -rf "$seed"
}
`

	prepareTmpfsLoopBody = `  mkdir -p "$dir" 2>/dev/null || true
  if ! touch "$dir/.write-test" 2>/dev/null; then
    tmp=$(mktemp -d)
    cp -a "$dir/." "$tmp/" 2>/dev/null || true
    mount -t tmpfs tmpfs "$dir"
    cp -a "$tmp/." "$dir/" 2>/dev/null || true
    rm -rf "$tmp"
  else
    rm -f "$dir/.write-test"
  fi
done`

	// hermesOwnershipFixup normalizes NemoClaw/Hermes seals under /sandbox after remount.
	// Product-specific; kept as an isolated fragment so it is obvious and easy to revisit.
	hermesOwnershipFixup = `# NemoClaw locked posture after remount (root:root looks like an orphaned seal).
chown root:sandbox /sandbox 2>/dev/null || true
chmod 1775 /sandbox 2>/dev/null || true
if [ -d /sandbox/.hermes ]; then
  # bootc numeric UIDs from the Debian stage can remap to wrong Fedora names;
  # normalize mutable trees to sandbox, then re-lock trust anchors.
  # Directory is root:sandbox sticky so the Landlock'd sandbox child can
  # atomic-replace .env / .config-hash without being able to unlink root seals.
  chown -R sandbox:sandbox /sandbox/.hermes 2>/dev/null || true
  chown root:sandbox /sandbox/.hermes 2>/dev/null || true
  chmod 1775 /sandbox/.hermes 2>/dev/null || true
  for f in config.yaml SOUL.md; do
    if [ -e "/sandbox/.hermes/$f" ]; then
      chown root:root "/sandbox/.hermes/$f" 2>/dev/null || true
      chmod 444 "/sandbox/.hermes/$f" 2>/dev/null || true
    fi
  done
  if [ -e /sandbox/.hermes/.config-hash ]; then
    chown sandbox:sandbox /sandbox/.hermes/.config-hash 2>/dev/null || true
    chmod 640 /sandbox/.hermes/.config-hash 2>/dev/null || true
  fi
  if [ -e /sandbox/.hermes/.env ]; then
    chown sandbox:sandbox /sandbox/.hermes/.env 2>/dev/null || true
    chmod 640 /sandbox/.hermes/.env 2>/dev/null || true
  fi
fi
mkdir -p /run/nemoclaw /run/openshell`
)

type cloudInitTLSData struct {
	CA   string
	Cert string
	Key  string
}

// sandboxContainerEnv is the OpenShell-relevant environment extracted from the
// sandbox pod template in a single pass.
type sandboxContainerEnv struct {
	Vars              []corev1.EnvVar
	Endpoint          string
	SandboxToken      string
	HasSandboxCommand bool
	SSHAuthorizedKeys []string
}

func readSandboxContainerEnv(sandbox *sandboxv1beta1.Sandbox) sandboxContainerEnv {
	var out sandboxContainerEnv
	for _, c := range sandbox.Spec.PodTemplate.Spec.Containers {
		for _, e := range c.Env {
			out.Vars = append(out.Vars, e)
			switch e.Name {
			case "OPENSHELL_ENDPOINT":
				out.Endpoint = e.Value
			case "OPENSHELL_SANDBOX_TOKEN":
				out.SandboxToken = e.Value
			case "OPENSHELL_SANDBOX_COMMAND":
				out.HasSandboxCommand = true
			case "OPENSHELL_SSH_AUTHORIZED_KEY":
				out.SSHAuthorizedKeys = append(out.SSHAuthorizedKeys, e.Value)
			}
		}
	}
	return out
}

func gatewayNamespaceFromEndpoint(endpoint string) string {
	// https://openshell.openshell.svc.cluster.local:8080 → "openshell"
	endpoint = strings.TrimPrefix(endpoint, "https://")
	endpoint = strings.TrimPrefix(endpoint, "http://")
	host := strings.Split(endpoint, ":")[0]
	parts := strings.Split(host, ".")
	if len(parts) >= 2 {
		return parts[1]
	}
	return "openshell"
}

func indentPEM(pem string) string {
	var sb strings.Builder
	for _, line := range strings.Split(strings.TrimRight(pem, "\n"), "\n") {
		sb.WriteString(cloudInitContentIndent)
		sb.WriteString(line)
		sb.WriteByte('\n')
	}
	return strings.TrimRight(sb.String(), "\n")
}

func (r *SandboxReconciler) createCloudInitSecret(ctx context.Context, sandbox *sandboxv1beta1.Sandbox, secretName, nameHash string, mounts []vmVolumeMount) error {
	logger := log.FromContext(ctx)

	env := readSandboxContainerEnv(sandbox)

	// When the gateway endpoint uses TLS (https://), the supervisor needs
	// client certs to connect back. Fetch them from the well-known
	// openshell-client-tls secret in the gateway namespace.
	var clientTLS *cloudInitTLSData
	if strings.HasPrefix(env.Endpoint, "https://") {
		gwNamespace := gatewayNamespaceFromEndpoint(env.Endpoint)
		tlsSecret := &corev1.Secret{}
		if err := r.Get(ctx, types.NamespacedName{Name: "openshell-client-tls", Namespace: gwNamespace}, tlsSecret); err != nil {
			logger.Info("Could not fetch client TLS secret for VM; sandbox-to-gateway TLS will not work",
				"namespace", gwNamespace, "error", err)
		} else {
			clientTLS = &cloudInitTLSData{
				CA:   string(tlsSecret.Data["ca.crt"]),
				Cert: string(tlsSecret.Data["tls.crt"]),
				Key:  string(tlsSecret.Data["tls.key"]),
			}
		}
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: sandbox.Namespace,
			Labels:    map[string]string{sandboxLabel: nameHash},
		},
		StringData: map[string]string{
			"userdata": buildCloudInitUserdataWithEnv(sandbox, env, clientTLS, mounts),
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

func buildCloudInitUserdata(sandbox *sandboxv1beta1.Sandbox, clientTLS *cloudInitTLSData, mounts []vmVolumeMount) string {
	return buildCloudInitUserdataWithEnv(sandbox, readSandboxContainerEnv(sandbox), clientTLS, mounts)
}

func buildCloudInitUserdataWithEnv(sandbox *sandboxv1beta1.Sandbox, env sandboxContainerEnv, clientTLS *cloudInitTLSData, mounts []vmVolumeMount) string {
	envBlock := buildOpenShellSystemdEnv(sandbox, env, clientTLS)
	writeFiles := buildCloudInitWriteFiles(sandbox, env, clientTLS)
	sshKeysYAML := buildSSHAuthorizedKeysYAML(env.SSHAuthorizedKeys)
	prepareScript := buildPrepareWritableRootsScript(mounts)
	systemdUnit := buildOpenShellSystemdUnit(envBlock)

	return fmt.Sprintf(`#cloud-config
ssh_pwauth: true
chpasswd:
  expire: false
  users:
  - name: sandbox
    password: sandbox
    type: text

users:
  - default
  - name: sandbox
    uid: "10001"
    shell: /bin/bash
    sudo: ALL=(ALL) NOPASSWD:ALL
    lock_passwd: false%s

# Drop console loglevel early so audit/info printk does not flood VNC/serial.
bootcmd:
  - [ sysctl, -w, kernel.printk=3 4 1 7 ]

write_files:
%s
  - path: /etc/sysctl.d/99-openshell-quiet-console.conf
    permissions: "0644"
    content: |
      # Keep audit and other info-level printk off the interactive console.
      kernel.printk = 3 4 1 7
  - path: /etc/openshell/prepare-writable-roots.sh
    permissions: "0755"
    content: |
%s
  - path: /etc/systemd/system/openshell-sandbox-prepare.service
    permissions: "0644"
    content: |
      [Unit]
      Description=Prepare writable roots for OpenShell sandbox
      DefaultDependencies=no
      After=local-fs.target
      Before=openshell-sandbox.service

      [Service]
      Type=oneshot
      RemainAfterExit=yes
      ExecStart=/etc/openshell/prepare-writable-roots.sh

      [Install]
      WantedBy=openshell-sandbox.service
%s

runcmd:
  - sysctl -p /etc/sysctl.d/99-openshell-quiet-console.conf || true
  - sed -i 's/^#\?Port .*/Port %d/' /etc/ssh/sshd_config
  - systemctl restart sshd || true
  - mkdir -p /run/openshell
  - systemctl daemon-reload
  - systemctl enable --now openshell-sandbox.service
`, sshKeysYAML, writeFiles, prepareScript, systemdUnit, sshPort)
}

func buildOpenShellSystemdEnv(sandbox *sandboxv1beta1.Sandbox, env sandboxContainerEnv, clientTLS *cloudInitTLSData) string {
	var envLines []string
	envLines = append(envLines, fmt.Sprintf("Environment=OPENSHELL_SANDBOX_ID=%s", sandbox.Name))
	envLines = append(envLines, fmt.Sprintf("Environment=OPENSHELL_SANDBOX=%s", sandbox.Name))
	envLines = append(envLines, "Environment=OPENSHELL_LOG_LEVEL=info")
	envLines = append(envLines, "Environment=OPENSHELL_SSH_SOCKET_PATH=/run/openshell/ssh.sock")
	envLines = append(envLines, "Environment=OPENSHELL_SANDBOX_UID=10001")
	envLines = append(envLines, "Environment=OPENSHELL_SANDBOX_GID=10001")

	for _, e := range env.Vars {
		switch e.Name {
		case "OPENSHELL_ENDPOINT":
			envLines = append(envLines, fmt.Sprintf("Environment=OPENSHELL_ENDPOINT=%s", e.Value))
		case "OPENSHELL_SANDBOX_TOKEN":
			// Token is written to a file; see buildCloudInitWriteFiles.
		case "OPENSHELL_SANDBOX_COMMAND":
			envLines = append(envLines, fmt.Sprintf("Environment=OPENSHELL_SANDBOX_COMMAND=%s", e.Value))
		default:
			envLines = append(envLines, fmt.Sprintf("Environment=%s=%s", e.Name, e.Value))
		}
	}

	if clientTLS != nil {
		envLines = append(envLines, fmt.Sprintf("Environment=OPENSHELL_TLS_CA=%s/ca.crt", clientTLSDir))
		envLines = append(envLines, fmt.Sprintf("Environment=OPENSHELL_TLS_CERT=%s/tls.crt", clientTLSDir))
		envLines = append(envLines, fmt.Sprintf("Environment=OPENSHELL_TLS_KEY=%s/tls.key", clientTLSDir))
	}

	if env.SandboxToken != "" {
		envLines = append(envLines, fmt.Sprintf("Environment=OPENSHELL_SANDBOX_TOKEN_FILE=%s", sandboxTokenPath))
	}
	if env.HasSandboxCommand {
		envLines = append(envLines, "Environment=OPENSHELL_PRESERVE_SANDBOX_OWNERSHIP=1")
	}

	return strings.Join(envLines, "\n      ")
}

func buildCloudInitWriteFiles(sandbox *sandboxv1beta1.Sandbox, env sandboxContainerEnv, clientTLS *cloudInitTLSData) string {
	var writeFiles strings.Builder
	fmt.Fprintf(&writeFiles, `  - path: /etc/openshell/sandbox-id
    content: "%s"
    permissions: "0644"
`, sandbox.Name)

	if env.SandboxToken != "" {
		fmt.Fprintf(&writeFiles, `  - path: %s
    content: "%s"
    permissions: "0400"
`, sandboxTokenPath, env.SandboxToken)
	}

	if clientTLS != nil {
		fmt.Fprintf(&writeFiles, `  - path: %s/ca.crt
    content: |
%s
    permissions: "0444"
  - path: %s/tls.crt
    content: |
%s
    permissions: "0444"
  - path: %s/tls.key
    content: |
%s
    permissions: "0400"
`, clientTLSDir, indentPEM(clientTLS.CA), clientTLSDir, indentPEM(clientTLS.Cert), clientTLSDir, indentPEM(clientTLS.Key))
	}

	return writeFiles.String()
}

func buildSSHAuthorizedKeysYAML(keys []string) string {
	if len(keys) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\n    ssh_authorized_keys:\n")
	for _, k := range keys {
		fmt.Fprintf(&sb, "    - %s\n", k)
	}
	return sb.String()
}

func buildOpenShellSystemdUnit(envBlock string) string {
	return fmt.Sprintf(`  - path: /etc/systemd/system/openshell-sandbox.service
    permissions: "0644"
    content: |
      [Unit]
      Description=OpenShell Sandbox Supervisor
      After=network-online.target sshd.service openshell-sandbox-prepare.service
      Wants=network-online.target
      Requires=openshell-sandbox-prepare.service

      [Service]
      Type=simple
      ExecStartPre=-/bin/bash -c 'for ns in $(ip netns list 2>/dev/null | grep "^sandbox-" | cut -d" " -f1); do ip netns delete "$ns" 2>/dev/null || true; done'
      ExecStart=%s
      Restart=on-failure
      RestartSec=5
      %s

      [Install]
      WantedBy=multi-user.target
`, supervisorBinary, envBlock)
}

// writeCloudInitScriptLines writes body lines with cloud-init content indent via w.
func writeCloudInitScriptLines(w func(string, ...interface{}), body string) {
	for _, line := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
		w("%s", line)
	}
}

// buildPrepareWritableRootsScript returns the body of prepare-writable-roots.sh
// indented for embedding under a cloud-init write_files content: | block.
// mounts are PVC-backed disks attached to the VM; those paths skip the tmpfs overlay.
func buildPrepareWritableRootsScript(mounts []vmVolumeMount) string {
	var sb strings.Builder
	w := func(format string, args ...interface{}) {
		sb.WriteString(cloudInitContentIndent)
		fmt.Fprintf(&sb, format, args...)
		sb.WriteByte('\n')
	}

	writeCloudInitScriptLines(w, prepareScriptHeader)
	w("")

	if len(mounts) > 0 {
		writeCloudInitScriptLines(w, prepareMountPVCDiskFunc)
		for _, m := range mounts {
			w("mount_pvc_disk %q %q", m.Serial, m.MountPath)
		}
		w("")
	}

	pvcPaths := make(map[string]struct{}, len(mounts))
	for _, m := range mounts {
		pvcPaths[m.MountPath] = struct{}{}
	}
	tmpfsDirs := make([]string, 0, 2)
	for _, dir := range []string{"/sandbox", "/opt/data"} {
		if _, skip := pvcPaths[dir]; skip {
			continue
		}
		tmpfsDirs = append(tmpfsDirs, dir)
	}
	if len(tmpfsDirs) > 0 {
		w("# tmpfs overlay for image paths that are not PVC-backed.")
		w("for dir in %s; do", strings.Join(tmpfsDirs, " "))
		writeCloudInitScriptLines(w, prepareTmpfsLoopBody)
	} else {
		w("# All default writable roots are PVC-backed; skipping tmpfs overlays.")
	}
	writeCloudInitScriptLines(w, hermesOwnershipFixup)

	return strings.TrimRight(sb.String(), "\n")
}
