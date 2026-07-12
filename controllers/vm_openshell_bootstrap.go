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

package controllers

import (
	"context"
	"fmt"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
)

// OpenShell gateway SA bootstrap for VirtualMachine sandboxes.
//
// Pod sandboxes use a kubelet-projected ServiceAccount token. VMs cannot, so
// the controller maintains a companion bootstrap Pod (Sandbox-owned, annotated
// with openshell.io/sandbox-id) and mints a BoundObjectRef TokenRequest into a
// Secret that is attached as a virtio disk. The guest exchanges that token via
// IssueSandboxToken the same way Pods do, including rebootstrap after reboot.
const (
	openshellSATokenVolumeName     = "openshell-sa-token"
	openshellSATokenKey            = "token"
	openshellSATokenAudience       = "openshell-gateway"
	openshellSATokenExpAnnotation  = "openshell.io/sa-token-expiration"
	openshellSandboxIDAnnotation   = "openshell.io/sandbox-id"
	openshellBootstrapPauseImage   = "registry.k8s.io/pause:3.10"
	openshellSATokenTTLSeconds     = int64(3600)
	openshellSATokenRefreshFloor   = time.Minute
)

func openshellSATokenSecretName(sandboxName string) string {
	return sandboxName + "-openshell-sa-token"
}

func openshellBootstrapPodName(sandboxName string) string {
	return sandboxName + "-openshell-bootstrap"
}

// wantsOpenshellSABootstrap is true when the Sandbox CR requests the OpenShell
// SA token Secret volume (driver-rendered volumeMount + secret volume).
func wantsOpenshellSABootstrap(sandbox *sandboxv1beta1.Sandbox) bool {
	wantSecret := openshellSATokenSecretName(sandbox.Name)
	for _, m := range collectVMSecretMounts(sandbox) {
		if m.Name == openshellSATokenVolumeName || m.SecretName == wantSecret {
			return true
		}
	}
	return false
}

func openshellSandboxID(sandbox *sandboxv1beta1.Sandbox) string {
	if sandbox.Spec.PodTemplate.ObjectMeta.Annotations != nil {
		if id := sandbox.Spec.PodTemplate.ObjectMeta.Annotations[openshellSandboxIDAnnotation]; id != "" {
			return id
		}
	}
	if sandbox.Labels != nil {
		if id := sandbox.Labels["openshell.ai/sandbox-id"]; id != "" {
			return id
		}
	}
	return ""
}

func openshellServiceAccountName(sandbox *sandboxv1beta1.Sandbox) string {
	if name := sandbox.Spec.PodTemplate.Spec.ServiceAccountName; name != "" {
		return name
	}
	return "default"
}

// reconcileOpenshellSABootstrap ensures the bootstrap Pod and rotating SA
// token Secret exist. Returns how long until the next token refresh.
func (r *SandboxReconciler) reconcileOpenshellSABootstrap(ctx context.Context, sandbox *sandboxv1beta1.Sandbox, nameHash string) (time.Duration, error) {
	logger := log.FromContext(ctx)

	sandboxID := openshellSandboxID(sandbox)
	if sandboxID == "" {
		return 0, fmt.Errorf("OpenShell SA bootstrap requires %s on podTemplate.metadata.annotations (or openshell.ai/sandbox-id label)", openshellSandboxIDAnnotation)
	}

	pod, err := r.ensureOpenshellBootstrapPod(ctx, sandbox, nameHash, sandboxID)
	if err != nil {
		return 0, err
	}
	if pod.UID == "" {
		return openshellSATokenRefreshFloor, nil
	}

	secretName := openshellSATokenSecretName(sandbox.Name)
	existing := &corev1.Secret{}
	err = r.Get(ctx, client.ObjectKey{Namespace: sandbox.Namespace, Name: secretName}, existing)
	if err != nil && !k8serrors.IsNotFound(err) {
		return 0, fmt.Errorf("get OpenShell SA token Secret: %w", err)
	}
	secretExists := err == nil

	if secretExists {
		if requeue, ok := saTokenStillFresh(existing); ok {
			return requeue, nil
		}
	}

	token, expiration, err := r.createOpenshellBoundToken(ctx, sandbox, pod)
	if err != nil {
		return 0, err
	}

	if err := r.upsertOpenshellSATokenSecret(ctx, sandbox, nameHash, secretName, token, expiration, secretExists, existing); err != nil {
		return 0, err
	}
	logger.Info("Refreshed OpenShell SA token Secret", "Secret.Name", secretName, "expires", expiration)

	return saTokenRequeueAfter(expiration), nil
}

func saTokenStillFresh(secret *corev1.Secret) (time.Duration, bool) {
	raw, ok := secret.Annotations[openshellSATokenExpAnnotation]
	if !ok || raw == "" {
		return 0, false
	}
	exp, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return 0, false
	}
	remaining := time.Until(exp)
	// Refresh when less than 20% of a nominal 1h TTL remains (~12m), matching
	// kubelet's projected-token refresh behaviour.
	if remaining > time.Duration(openshellSATokenTTLSeconds)*time.Second/5 {
		return saTokenRequeueAfter(exp), true
	}
	return 0, false
}

func saTokenRequeueAfter(expiration time.Time) time.Duration {
	until := time.Until(expiration) * 4 / 5
	if until < openshellSATokenRefreshFloor {
		return openshellSATokenRefreshFloor
	}
	return until
}

func (r *SandboxReconciler) ensureOpenshellBootstrapPod(ctx context.Context, sandbox *sandboxv1beta1.Sandbox, nameHash, sandboxID string) (*corev1.Pod, error) {
	logger := log.FromContext(ctx)
	name := openshellBootstrapPodName(sandbox.Name)

	pod := &corev1.Pod{}
	err := r.Get(ctx, client.ObjectKey{Namespace: sandbox.Namespace, Name: name}, pod)
	if err == nil {
		return pod, nil
	}
	if !k8serrors.IsNotFound(err) {
		return nil, fmt.Errorf("get OpenShell bootstrap Pod: %w", err)
	}

	saName := openshellServiceAccountName(sandbox)
	pod = &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: sandbox.Namespace,
			Labels: map[string]string{
				sandboxLabel: nameHash,
			},
			Annotations: map[string]string{
				openshellSandboxIDAnnotation: sandboxID,
			},
		},
		Spec: corev1.PodSpec{
			ServiceAccountName:            saName,
			AutomountServiceAccountToken:  ptr.To(false),
			RestartPolicy:                 corev1.RestartPolicyAlways,
			TerminationGracePeriodSeconds: ptr.To(int64(1)),
			Containers: []corev1.Container{{
				Name:  "pause",
				Image: openshellBootstrapPauseImage,
			}},
		},
	}
	if err := ctrl.SetControllerReference(sandbox, pod, r.Scheme); err != nil {
		return nil, fmt.Errorf("SetControllerReference for OpenShell bootstrap Pod: %w", err)
	}
	if err := r.Create(ctx, pod, client.FieldOwner(sandboxControllerFieldOwner)); err != nil {
		if !k8serrors.IsAlreadyExists(err) {
			return nil, fmt.Errorf("create OpenShell bootstrap Pod: %w", err)
		}
	} else {
		logger.Info("Created OpenShell bootstrap Pod", "Pod.Name", name)
	}
	// Re-get for UID (BoundObjectRef requires it).
	if err := r.Get(ctx, client.ObjectKey{Namespace: sandbox.Namespace, Name: name}, pod); err != nil {
		return nil, fmt.Errorf("get OpenShell bootstrap Pod after create: %w", err)
	}
	return pod, nil
}

func (r *SandboxReconciler) createOpenshellBoundToken(ctx context.Context, sandbox *sandboxv1beta1.Sandbox, pod *corev1.Pod) (string, time.Time, error) {
	saName := openshellServiceAccountName(sandbox)
	sa := &corev1.ServiceAccount{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: sandbox.Namespace, Name: saName}, sa); err != nil {
		return "", time.Time{}, fmt.Errorf("get ServiceAccount %q for OpenShell bootstrap: %w", saName, err)
	}

	tr := &authenticationv1.TokenRequest{
		Spec: authenticationv1.TokenRequestSpec{
			Audiences:         []string{openshellSATokenAudience},
			ExpirationSeconds: ptr.To(openshellSATokenTTLSeconds),
			BoundObjectRef: &authenticationv1.BoundObjectReference{
				APIVersion: "v1",
				Kind:       "Pod",
				Name:       pod.Name,
				UID:        pod.UID,
			},
		},
	}
	if err := r.SubResource("token").Create(ctx, sa, tr); err != nil {
		return "", time.Time{}, fmt.Errorf("TokenRequest for OpenShell bootstrap: %w", err)
	}
	if tr.Status.Token == "" {
		return "", time.Time{}, fmt.Errorf("TokenRequest returned empty token")
	}
	exp := tr.Status.ExpirationTimestamp.Time
	if exp.IsZero() {
		exp = time.Now().Add(time.Duration(openshellSATokenTTLSeconds) * time.Second)
	}
	return tr.Status.Token, exp, nil
}

func (r *SandboxReconciler) upsertOpenshellSATokenSecret(
	ctx context.Context,
	sandbox *sandboxv1beta1.Sandbox,
	nameHash, secretName, token string,
	expiration time.Time,
	exists bool,
	existing *corev1.Secret,
) error {
	annotations := map[string]string{
		openshellSATokenExpAnnotation: expiration.UTC().Format(time.RFC3339),
	}
	data := map[string][]byte{
		openshellSATokenKey: []byte(token),
	}

	if !exists {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:        secretName,
				Namespace:   sandbox.Namespace,
				Labels:      map[string]string{sandboxLabel: nameHash},
				Annotations: annotations,
			},
			Type: corev1.SecretTypeOpaque,
			Data: data,
		}
		if err := ctrl.SetControllerReference(sandbox, secret, r.Scheme); err != nil {
			return fmt.Errorf("SetControllerReference for OpenShell SA token Secret: %w", err)
		}
		if err := r.Create(ctx, secret, client.FieldOwner(sandboxControllerFieldOwner)); err != nil {
			if k8serrors.IsAlreadyExists(err) {
				return nil
			}
			return fmt.Errorf("create OpenShell SA token Secret: %w", err)
		}
		return nil
	}

	patch := client.MergeFrom(existing.DeepCopy())
	if existing.Labels == nil {
		existing.Labels = map[string]string{}
	}
	existing.Labels[sandboxLabel] = nameHash
	if existing.Annotations == nil {
		existing.Annotations = map[string]string{}
	}
	existing.Annotations[openshellSATokenExpAnnotation] = annotations[openshellSATokenExpAnnotation]
	existing.Data = data
	existing.StringData = nil
	if err := r.Patch(ctx, existing, patch, client.FieldOwner(sandboxControllerFieldOwner)); err != nil {
		return fmt.Errorf("patch OpenShell SA token Secret: %w", err)
	}
	return nil
}
