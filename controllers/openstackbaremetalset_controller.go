/*
Copyright 2023.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"fmt"
	"time"

	k8s_errors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/go-logr/logr"
	metal3v1alpha1 "github.com/metal3-io/baremetal-operator/apis/metal3.io/v1alpha1"
	"github.com/openstack-k8s-operators/lib-common/modules/common"
	"github.com/openstack-k8s-operators/lib-common/modules/common/condition"
	"github.com/openstack-k8s-operators/lib-common/modules/common/env"
	"github.com/openstack-k8s-operators/lib-common/modules/common/helper"
	"github.com/openstack-k8s-operators/lib-common/modules/common/labels"
	oko_secret "github.com/openstack-k8s-operators/lib-common/modules/common/secret"
	baremetalv1 "github.com/openstack-k8s-operators/openstack-baremetal-operator/api/v1beta1"
	"github.com/openstack-k8s-operators/openstack-baremetal-operator/pkg/openstackbaremetalset"
	openstackprovisionserver "github.com/openstack-k8s-operators/openstack-baremetal-operator/pkg/openstackprovisionserver"
)

// OpenStackBaremetalSetReconciler reconciles a OpenStackBaremetalSet object
type OpenStackBaremetalSetReconciler struct {
	client.Client
	Kclient kubernetes.Interface
	Log     logr.Logger
	Scheme  *runtime.Scheme
}

// +kubebuilder:rbac:groups=baremetal.openstack.org,resources=openstackbaremetalsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=baremetal.openstack.org,resources=openstackbaremetalsets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=baremetal.openstack.org,resources=openstackbaremetalsets/finalizers,verbs=update
// +kubebuilder:rbac:groups=baremetal.openstack.org,resources=openstackprovisionservers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=baremetal.openstack.org,resources=openstackprovisionservers/finalizers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=baremetal.openstack.org,resources=openstackprovisionservers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=metal3.io,resources=baremetalhosts,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=metal3.io,resources=baremetalhosts/status,verbs=get
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=create;delete;get;list;patch;update;watch
// +kubebuilder:rbac:groups=core,resources=secrets/finalizers,verbs=create;delete;get;list;patch;update;watch

// Reconcile -
func (r *OpenStackBaremetalSetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, _err error) {
	_ = log.FromContext(ctx)

	// Fetch the OpenStackBaremetalSet instance
	instance := &baremetalv1.OpenStackBaremetalSet{}
	err := r.Client.Get(ctx, req.NamespacedName, instance)
	if err != nil {
		if k8s_errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected.
			// For additional cleanup logic use finalizers. Return and don't requeue.
			return ctrl.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return ctrl.Result{}, err
	}

	helper, err := helper.NewHelper(
		instance,
		r.Client,
		r.Kclient,
		r.Scheme,
		r.Log,
	)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Always patch the instance status when exiting this function so we can persist any changes.
	defer func() {
		// update the overall status condition if service is ready
		if instance.IsReady() {
			instance.Status.Conditions.MarkTrue(condition.ReadyCondition, condition.ReadyMessage)
		}

		err := helper.PatchInstance(ctx, instance)
		if err != nil {
			_err = err
			return
		}
	}()

	// If we're not deleting this and the service object doesn't have our finalizer, add it.
	if instance.DeletionTimestamp.IsZero() && controllerutil.AddFinalizer(instance, helper.GetFinalizer()) {
		return ctrl.Result{}, nil
	}

	//
	// initialize status
	//
	if instance.Status.Conditions == nil {
		instance.Status.Conditions = condition.Conditions{}
		// initialize conditions used later as Status=Unknown
		cl := condition.CreateList(
			condition.UnknownCondition(condition.InputReadyCondition, condition.InitReason, condition.InputReadyInitMessage),
			condition.UnknownCondition(baremetalv1.OpenStackBaremetalSetProvServerReadyCondition, condition.InitReason, baremetalv1.OpenStackBaremetalSetProvServerReadyInitMessage),
		)

		instance.Status.Conditions.Init(&cl)

		// Register overall status immediately to have an early feedback e.g. in the cli
		return ctrl.Result{}, nil
	}
	if instance.Status.Hash == nil {
		instance.Status.Hash = map[string]string{}
	}

	// Handle service delete
	if !instance.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, instance, helper)
	}

	// Handle non-deleted servers
	return r.reconcileNormal(ctx, instance, helper)
}

// SetupWithManager sets up the controller with the Manager.
func (r *OpenStackBaremetalSetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	openshiftMachineAPIBareMetalHostsFn := handler.EnqueueRequestsFromMapFunc(func(o client.Object) []reconcile.Request {
		// TODO
		return nil
	})

	return ctrl.NewControllerManagedBy(mgr).
		For(&baremetalv1.OpenStackBaremetalSet{}).
		Owns(&baremetalv1.OpenStackBaremetalSet{}).
		Watches(&source.Kind{Type: &metal3v1alpha1.BareMetalHost{}}, openshiftMachineAPIBareMetalHostsFn).
		Complete(r)
}

func (r *OpenStackBaremetalSetReconciler) reconcileDelete(ctx context.Context, instance *baremetalv1.OpenStackBaremetalSet, helper *helper.Helper) (ctrl.Result, error) {
	r.Log.Info(fmt.Sprintf("Reconciling OpenStackBaremetalSet '%s' delete", instance.Name))

	controllerutil.RemoveFinalizer(instance, helper.GetFinalizer())
	r.Log.Info(fmt.Sprintf("Reconciled OpenStackBaremetalSet '%s' delete successfully", instance.Name))

	return ctrl.Result{}, nil
}

func (r *OpenStackBaremetalSetReconciler) reconcileInit(
	ctx context.Context,
	instance *baremetalv1.OpenStackBaremetalSet,
	helper *helper.Helper,
	serviceLabels map[string]string,
) (ctrl.Result, error) {
	r.Log.Info(fmt.Sprintf("Reconciling OpenStackBaremetalSet '%s' init", instance.Name))

	r.Log.Info(fmt.Sprintf("Reconciled OpenStackBaremetalSet '%s' init successfully", instance.Name))
	return ctrl.Result{}, nil
}

func (r *OpenStackBaremetalSetReconciler) reconcileUpdate(ctx context.Context, instance *baremetalv1.OpenStackBaremetalSet, helper *helper.Helper) (ctrl.Result, error) {
	r.Log.Info(fmt.Sprintf("Reconciling OpenStackBaremetalSet '%s' update", instance.Name))

	// TODO: should have minor update tasks if required
	// - delete dbsync hash from status to rerun it?

	r.Log.Info(fmt.Sprintf("Reconciled OpenStackBaremetalSet '%s' update successfully", instance.Name))
	return ctrl.Result{}, nil
}

func (r *OpenStackBaremetalSetReconciler) reconcileUpgrade(ctx context.Context, instance *baremetalv1.OpenStackBaremetalSet, helper *helper.Helper) (ctrl.Result, error) {
	r.Log.Info(fmt.Sprintf("Reconciling OpenStackBaremetalSet '%s' upgrade", instance.Name))

	// TODO: should have major version upgrade tasks
	// -delete dbsync hash from status to rerun it?

	r.Log.Info(fmt.Sprintf("Reconciled OpenStackBaremetalSet '%s' upgrade successfully", instance.Name))
	return ctrl.Result{}, nil
}

func (r *OpenStackBaremetalSetReconciler) reconcileNormal(ctx context.Context, instance *baremetalv1.OpenStackBaremetalSet, helper *helper.Helper) (ctrl.Result, error) {
	r.Log.Info(fmt.Sprintf("Reconciling OpenStackBaremetalSet '%s'", instance.Name))

	l := log.FromContext(ctx)

	// ConfigMap
	configMapVars := make(map[string]env.Setter)

	//
	// check if a root password secret was provide and add hash to the vars map if so
	//
	if instance.Spec.PasswordSecret != "" {
		ospSecret, hash, err := oko_secret.GetSecret(ctx, helper, instance.Spec.PasswordSecret, instance.Namespace)
		if err != nil {
			if k8s_errors.IsNotFound(err) {
				instance.Status.Conditions.Set(condition.FalseCondition(
					condition.InputReadyCondition,
					condition.RequestedReason,
					condition.SeverityInfo,
					condition.InputReadyWaitingMessage))
				return ctrl.Result{RequeueAfter: time.Second * 10}, fmt.Errorf("Root password secret %s not found", instance.Spec.PasswordSecret)
			}
			instance.Status.Conditions.Set(condition.FalseCondition(
				condition.InputReadyCondition,
				condition.ErrorReason,
				condition.SeverityWarning,
				condition.InputReadyErrorMessage,
				err.Error()))
			return ctrl.Result{}, err
		}
		configMapVars[ospSecret.Name] = env.SetValue(hash)
	}
	// run check OpenStack secret - end

	//
	// check if the required deployment SSH secret is available and add hash to the vars map
	//
	ospSecret, hash, err := oko_secret.GetSecret(ctx, helper, instance.Spec.DeploymentSSHSecret, instance.Namespace)
	if err != nil {
		if k8s_errors.IsNotFound(err) {
			instance.Status.Conditions.Set(condition.FalseCondition(
				condition.InputReadyCondition,
				condition.RequestedReason,
				condition.SeverityInfo,
				condition.InputReadyWaitingMessage))
			return ctrl.Result{RequeueAfter: time.Second * 10}, fmt.Errorf("Deployment SSH secret %s not found", instance.Spec.DeploymentSSHSecret)
		}
		instance.Status.Conditions.Set(condition.FalseCondition(
			condition.InputReadyCondition,
			condition.ErrorReason,
			condition.SeverityWarning,
			condition.InputReadyErrorMessage,
			err.Error()))
		return ctrl.Result{}, err
	}
	configMapVars[ospSecret.Name] = env.SetValue(hash)
	instance.Status.Conditions.MarkTrue(condition.InputReadyCondition, condition.InputReadyMessage)
	// run check deployment SSH secret - end

	//
	// TODO check when/if Init, Update, or Upgrade should/could be skipped
	//

	serviceLabels := labels.GetLabels(instance, openstackprovisionserver.AppLabel, map[string]string{
		common.AppSelector: instance.Name + "-openstackbaremetalset-deployment",
	})

	// Handle service init
	ctrlResult, err := r.reconcileInit(ctx, instance, helper, serviceLabels)
	if err != nil {
		return ctrlResult, err
	} else if (ctrlResult != ctrl.Result{}) {
		return ctrlResult, nil
	}

	// Handle service update
	ctrlResult, err = r.reconcileUpdate(ctx, instance, helper)
	if err != nil {
		return ctrlResult, err
	} else if (ctrlResult != ctrl.Result{}) {
		return ctrlResult, nil
	}

	// Handle service upgrade
	ctrlResult, err = r.reconcileUpgrade(ctx, instance, helper)
	if err != nil {
		return ctrlResult, err
	} else if (ctrlResult != ctrl.Result{}) {
		return ctrlResult, nil
	}

	//
	// normal reconcile tasks
	//

	//
	// either find the provided provision server or create a new one
	//
	var provisionServer *baremetalv1.OpenStackProvisionServer

	// TODO: webook should validate that either ProvisionServerName or RhelImageUrl is set in the instance spec
	if instance.Spec.ProvisionServerName == "" {
		provisionServer, err = openstackbaremetalset.ProvisionServerCreateOrUpdate(ctx, helper, instance)
	} else {
		err = r.Client.Get(ctx, types.NamespacedName{Name: instance.Spec.ProvisionServerName, Namespace: instance.Namespace}, provisionServer)
	}

	if err != nil {
		if k8s_errors.IsNotFound(err) {
			instance.Status.Conditions.Set(condition.FalseCondition(
				baremetalv1.OpenStackBaremetalSetProvServerReadyCondition,
				condition.RequestedReason,
				condition.SeverityInfo,
				baremetalv1.OpenStackBaremetalSetProvServerReadyWaitingMessage))
			return ctrl.Result{RequeueAfter: time.Duration(10) * time.Second}, fmt.Errorf("OpenStackProvisionServer %s not found", instance.Spec.ProvisionServerName)
		}
		instance.Status.Conditions.Set(condition.FalseCondition(
			baremetalv1.OpenStackBaremetalSetProvServerReadyCondition,
			condition.ErrorReason,
			condition.SeverityWarning,
			baremetalv1.OpenStackBaremetalSetProvServerReadyErrorMessage,
			err.Error()))
		return ctrl.Result{}, err
	}

	if provisionServer.Status.LocalImageURL == "" {
		instance.Status.Conditions.Set(condition.FalseCondition(
			baremetalv1.OpenStackBaremetalSetProvServerReadyCondition,
			condition.RequestedReason,
			condition.SeverityInfo,
			baremetalv1.OpenStackBaremetalSetProvServerReadyRunningMessage))
		l.Info("OpenStackProvisionServer LocalImageUrl not yet available", "OpenStackProvisionServer", provisionServer.Name)
		return ctrl.Result{RequeueAfter: time.Duration(30) * time.Second}, nil
	}
	instance.Status.Conditions.MarkTrue(baremetalv1.OpenStackBaremetalSetProvServerReadyCondition, baremetalv1.OpenStackBaremetalSetProvServerReadyMessage)
	// handle provision server - end

	r.Log.Info(fmt.Sprintf("Reconciled OpenStackBaremetalSet '%s' successfully", instance.Name))
	return ctrl.Result{}, nil
}
