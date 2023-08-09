//
// Copyright (c) 2023 Red Hat, Inc.
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

	appv1 "github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	api "github.com/redhat-appstudio/remote-secret/api/v1beta1"
	"github.com/redhat-appstudio/remote-secret/pkg/logs"
)

const (
	argoApplicationAnnotation = "appstudio.redhat.com/remotesecret-argocd-application"
)

type ArgoCDApplicationReconciler struct {
	client.Client
}

// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;watch;list
// +kubebuilder:rbac:groups=argoproj.io,resources=applications,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=appstudio.redhat.com,resources=remotesecrets,verbs=get;list;watch;create;update;patch;delete

var _ reconcile.Reconciler = (*ArgoCDApplicationReconciler)(nil)

func (r *ArgoCDApplicationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	err := ctrl.NewControllerManagedBy(mgr).For(&appv1.Application{}).Complete(r)
	if err != nil {
		return fmt.Errorf("failed to initialize the controller: %w", err)
	}

	return nil
}

// The main idea of the Application reconciler is to detect all the secrets in the application, define a remote secret in
// the application namespace with properly set up target for each of them and set up the application to ignore
// the differences in the data of the secrets.
func (r *ArgoCDApplicationReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	lg := log.FromContext(ctx)
	lg.V(logs.DebugLevel).Info("starting reconciliation")
	defer logs.TimeTrackWithLazyLogger(func() logr.Logger { return lg }, time.Now(), "Reconcile ArgoCD Application")

	application := &appv1.Application{}

	if err := r.Get(ctx, req.NamespacedName, application); err != nil {
		if errors.IsNotFound(err) {
			lg.V(logs.DebugLevel).Info("RemoteSecret already gone from the cluster. skipping reconciliation")
			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, fmt.Errorf("failed to get the RemoteSecret: %w", err)
	}

	if application.DeletionTimestamp != nil {
		lg.V(logs.DebugLevel).Info("RemoteSecret is being deleted. skipping reconciliation")
		return ctrl.Result{}, nil
	}

	remoteSecrets := &api.RemoteSecretList{}
	if err := r.List(ctx, remoteSecrets, client.InNamespace(application.Namespace)); err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to list remote secrets in the namespace %s: %w", req.Namespace, err)
	}

	remoteSecretsBySecret := map[types.NamespacedName]*api.RemoteSecret{}

	for i := range remoteSecrets.Items {
		rs := remoteSecrets.Items[i]
		if rs.Annotations[argoApplicationAnnotation] != application.Name {
			continue
		}
		for _, t := range rs.Spec.Targets {
			remoteSecretsBySecret[types.NamespacedName{Name: rs.Spec.Secret.Name, Namespace: t.Namespace}] = &rs
		}
	}

	// first tell ArgoCD to ignore the data in all the secrets we are going to replace with data from remote secrets.
	for _, res := range application.Status.Resources {
		if res.Kind != "Secret" || res.Group != "" || res.Version != "v1" {
			continue
		}

		if res.Status != appv1.SyncStatusCodeSynced {
			// we need the secrets to be synced first, so that we can see how they look like in the target so that we can replicate it
			// in the remote secret.
			continue
		}

		r.ignoreSecretDataInApplication(res, application)
	}

	// set the sync policy to respect the ignores also during the sync, so that ArgoCD doesn't overwrite our changes to the secret data on the next sync.
	// TODO: is this too invasive?
	application.Spec.SyncPolicy.SyncOptions.AddOption("RespectIgnoreDifferences=true")

	if err := r.Update(ctx, application); err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to update the ArgoCD application %s to ignore the secret data: %w", client.ObjectKeyFromObject(application), err)
	}

	// now, let's go through the secrets again and make sure there are properly set up remote secrets for each of them.
	targetClient := r.getClientForApplication(application)

	for _, res := range application.Status.Resources {
		if res.Kind != "Secret" || res.Group != "" || res.Version != "v1" {
			continue
		}

		if res.Status != appv1.SyncStatusCodeSynced {
			// we need the secrets to be synced first, so that we can see how they look like in the target so that we can replicate it
			// in the remote secret.
			continue
		}

		// This is a bit ugly... Ideally, we would be able to see the secrets in the ArgoCD database so that we don't have to reach out to the cluster
		// but this is quick and simple. Or we could just give up on validation and just put whatever there is in the remote secret to the target secret
		// and be done with it (this would require subtle changes in the remote secret so that we can tell it not to validate the secret shape).
		targetSecret := &corev1.Secret{}
		if err := targetClient.Get(ctx, client.ObjectKey{Name: res.Name, Namespace: res.Namespace}, targetSecret); err != nil {
			return reconcile.Result{}, fmt.Errorf("failed to obtain the secret from the target: %w", err)
		}

		rs, ok := remoteSecretsBySecret[types.NamespacedName{Name: res.Name, Namespace: res.Namespace}]
		if !ok {
			// there is no remote secret for this secret
			rs := &api.RemoteSecret{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: res.Name + "-",
					Namespace:    application.Namespace,
					Annotations: map[string]string{
						argoApplicationAnnotation: application.Name,
					},
				},
				Spec: api.RemoteSecretSpec{
					Secret: api.LinkableSecretSpec{
						Name: res.Name,
						Type: targetSecret.Type,
						// TODO: set up the required keys so that they exactly match the secret in the target.
					},
					Targets: []api.RemoteSecretTarget{
						{
							Namespace: res.Namespace,
							// TODO: this needs to also create the kubeconfig similarly to how we obtained the client above
							// alternatively, we need to teach Remote secrets to understand the cluster configuration secrets of ArgoCD
							// and reference it here...
						},
					},
				},
			}

			if err := r.Create(ctx, rs); err != nil {
				return reconcile.Result{}, fmt.Errorf("failed to create the remote secret to provide data for the secret %s: %w", client.ObjectKey{Name: res.Name, Namespace: res.Namespace}, err)
			}
		} else {
			rs.Spec.Secret.Type = targetSecret.Type
			// TODO: sync the keys, too
			if err := r.Update(ctx, rs); err != nil {
				return reconcile.Result{}, fmt.Errorf("failed to update the remote secret: %w", err)
			}
		}
	}
	return reconcile.Result{}, nil
}

func (r *ArgoCDApplicationReconciler) getClientForApplication(application *appv1.Application) client.Client {
	// TODO: determine the client to use for the application
	// ArgoCD uses secrets in the argocd namespace for the clster definitions where the connection configurations
	// are stored and we need to look that for any non-local cluster. For now, let's just not and only support deployment
	// to the local cluster. Let's therefore use our client.
	return r.Client
}

func (r *ArgoCDApplicationReconciler) ignoreSecretDataInApplication(res appv1.ResourceStatus, application *appv1.Application) {
	for _, ignore := range application.Spec.IgnoreDifferences {
		if ignore.Group == res.Group && ignore.Kind == res.Kind && ignore.Name == res.Name && ignore.Namespace == res.Namespace {
			return
		}
	}

	application.Spec.IgnoreDifferences = append(application.Spec.IgnoreDifferences, appv1.ResourceIgnoreDifferences{
		Group:        res.Group,
		Kind:         res.Kind,
		Name:         res.Name,
		Namespace:    res.Namespace,
		JSONPointers: []string{"/data"},
	})
}
