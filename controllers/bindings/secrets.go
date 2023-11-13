//
// Copyright (c) 2021 Red Hat, Inc.
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

package bindings

import (
	"context"
	"fmt"

	"github.com/redhat-appstudio/remote-secret/api/v1beta1"
	"github.com/redhat-appstudio/remote-secret/pkg/logs"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type secretHandler[K any] struct {
	Target           SecretDeploymentTarget
	ObjectMarker     ObjectMarker
	SecretDataGetter SecretDataGetter[K]
}

// GetStale detects whether the secret referenced by the target is stale and needs to be replaced by a new one.
// A secret in the target can become stale if it no longer corresponds to the spec of the target.
func (h *secretHandler[K]) GetStale(ctx context.Context) (*corev1.Secret, error) {
	existingSecretName := h.Target.GetActualSecretName()
	spec := h.Target.GetSpec()
	if existingSecretName == "" || NameCorresponds(existingSecretName, spec.Name, spec.GenerateName) {
		return nil, nil
	}

	existingSecret := &corev1.Secret{}
	err := h.Target.GetClient().Get(ctx, client.ObjectKey{Name: existingSecretName, Namespace: h.Target.GetTargetNamespace()}, existingSecret)
	if errors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return existingSecret, fmt.Errorf("failed to detect whether the secret is stale: %w", err)
	} else {
		return existingSecret, nil
	}
}

// Sync creates or updates the secret with the data from the given key. The recreate flag can be used to force the creation of a new secret even
// if the target already reports an existing secret using its GetActualSecretName method. This can be used to deal with the stale secrets (see GetStale method).
func (h *secretHandler[K]) Sync(ctx context.Context, key K, recreate bool) (*corev1.Secret, string, error) {
	data, errorReason, err := h.SecretDataGetter.GetData(ctx, key)
	if err != nil {
		return nil, errorReason, fmt.Errorf("failed to obtain the secret data: %w", err)
	}

	spec := h.Target.GetSpec()

	secretName := h.Target.GetActualSecretName()
	if recreate || secretName == "" {
		secretName = spec.Name
	}

	secretGenerateName := spec.GenerateName
	if secretGenerateName == "" {
		secretGenerateName = h.Target.GetTargetObjectKey().Name + "-secret-"
	}

	var secret *corev1.Secret

	if secretName == "" {
		// create the secret
		if secret, err = h.createTargetSecret(ctx, key, secretName, secretGenerateName, spec, data); err != nil {
			return nil, string(ErrorReasonSecretUpdate), fmt.Errorf("failed to create the target secret of the deployment target %s (%s): %w", h.Target.GetTargetObjectKey(), h.Target.GetType(), err)
		}
	} else {
		// update the secret
		if secret, err = h.updateTargetSecret(ctx, key, secretName, secretGenerateName, spec, data); err != nil {
			return nil, string(ErrorReasonSecretUpdate), fmt.Errorf("failed to update the target secret of the deployment target %s (%s): %w", h.Target.GetTargetObjectKey(), h.Target.GetType(), err)
		}
	}

	return secret, "", nil
}

func (h *secretHandler[K]) createTargetSecret(ctx context.Context, key K, secretName string, secretGenerateName string, spec v1beta1.LinkableSecretSpec, data map[string][]byte) (*corev1.Secret, error) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:         secretName,
			GenerateName: secretGenerateName,
			Namespace:    h.Target.GetTargetNamespace(),
			Labels:       spec.Labels,
			Annotations:  spec.Annotations,
		},
		Data: data,
		Type: spec.Type,
	}

	var err error

	_, err = h.ObjectMarker.MarkManaged(ctx, h.Target.GetTargetObjectKey(), secret)
	if err != nil {
		return nil, err //nolint: wrapcheck // this is wrapped at the callsites
	}
	if err = h.Target.GetClient().Create(ctx, secret); err != nil {
		if errors.IsAlreadyExists(err) {
			return h.updateTargetSecret(ctx, key, secretName, secretGenerateName, spec, data)
		}
		return nil, err //nolint: wrapcheck // this is wrapped at the callsites
	}
	return secret, nil
}

func (h *secretHandler[K]) updateTargetSecret(ctx context.Context, key K, secretName string, secretGenerateName string, spec v1beta1.LinkableSecretSpec, data map[string][]byte) (*corev1.Secret, error) {
	secret := &corev1.Secret{}
	if err := h.Target.GetClient().Get(ctx, client.ObjectKey{Name: secretName, Namespace: h.Target.GetTargetNamespace()}, secret); err != nil {
		if errors.IsNotFound(err) {
			if secret, err = h.createTargetSecret(ctx, key, secretName, secretGenerateName, spec, data); err != nil {
				return nil, err
			}
		} else {
			return nil, err
		}
	}
	_, err := h.ObjectMarker.MarkManaged(ctx, h.Target.GetTargetObjectKey(), secret)
	if err != nil {
		return nil, err //nolint: wrapcheck // this is wrapped at the callsites
	}

	// set the labels, annos and data fields that we require. Leave everything else in place.
	if secret.Labels == nil {
		secret.Labels = map[string]string{}
	}
	if secret.Annotations == nil {
		secret.Annotations = map[string]string{}
	}
	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}

	for k, v := range spec.Labels {
		secret.Labels[k] = v
	}
	for k, v := range spec.Annotations {
		secret.Annotations[k] = v
	}
	for k, v := range data {
		secret.Data[k] = v
	}

	// and update
	if err = h.Target.GetClient().Update(ctx, secret); err != nil {
		if errors.IsNotFound(err) {
			if secret, err = h.createTargetSecret(ctx, key, secretName, secretGenerateName, spec, data); err != nil {
				return nil, err
			}
		} else {
			return nil, err
		}
	}
	return secret, nil
}

func (h *secretHandler[K]) List(ctx context.Context) ([]*corev1.Secret, error) {
	sl := &corev1.SecretList{}
	opts, err := h.ObjectMarker.ListManagedOptions(ctx, h.Target.GetTargetObjectKey())
	if err != nil {
		return nil, fmt.Errorf("failed to formulate the options to list the secrets in the deployment target (%s): %w", h.Target.GetType(), err)
	}

	opts = append(opts, client.InNamespace(h.Target.GetTargetNamespace()))

	lg := log.FromContext(ctx).V(logs.DebugLevel)
	if err := h.Target.GetClient().List(ctx, sl, opts...); err != nil {
		return []*corev1.Secret{}, fmt.Errorf("failed to list the secrets associated with the deployment target (%s) %+v: %w", h.Target.GetType(), h.Target.GetTargetObjectKey(), err)
	}

	lg.Info("listing secrets managed by target", "targetType", h.Target.GetType(), "targetKey", h.Target.GetTargetObjectKey(), "targetNamespace", h.Target.GetTargetNamespace(), "opts", opts, "secretCount", len(sl.Items))

	ret := []*corev1.Secret{}
	for i := range sl.Items {
		if ok, err := h.ObjectMarker.IsManagedBy(ctx, h.Target.GetTargetObjectKey(), &sl.Items[i]); err != nil {
			return []*corev1.Secret{}, fmt.Errorf("failed to determine if the secret %s is managed while processing the deployment target (%s) %s: %w",
				client.ObjectKeyFromObject(&sl.Items[i]),
				h.Target.GetType(),
				h.Target.GetTargetObjectKey(),
				err)
		} else if ok {
			ret = append(ret, &sl.Items[i])
		}
	}

	return ret, nil
}
