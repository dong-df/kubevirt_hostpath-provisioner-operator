/*
Copyright 2019 The hostpath provisioner operator Authors.

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

package hostpathprovisioner

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/go-logr/logr"
	ocpconfigv1 "github.com/openshift/api/config/v1"
	secv1 "github.com/openshift/api/security/v1"
	conditions "github.com/openshift/custom-resource-status/conditions/v1"
	promv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	hostpathprovisionerv1 "kubevirt.io/hostpath-provisioner-operator/pkg/apis/hostpathprovisioner/v1beta1"
	"kubevirt.io/hostpath-provisioner-operator/pkg/monitoring/metrics"
	"kubevirt.io/hostpath-provisioner-operator/pkg/util/cryptopolicy"
	"kubevirt.io/hostpath-provisioner-operator/version"
)

var (
	log                = logf.Log.WithName("controller_hostpathprovisioner")
	watchNamespaceFunc = GetNamespace
)

func init() {
	err := metrics.SetupMetrics()
	if err != nil {
		panic(err)
	}
	// 0 is our 'something bad is going on' value for alert to start firing, so can't default to that
	metrics.SetReadyGaugeValue(-1)
}

const (
	snapshotFeatureGate = "Snapshotting"
	hppFinalizer        = "finalizer.delete.hostpath-provisioner"
)

func isErrCacheNotStarted(err error) bool {
	if err == nil {
		return false
	}
	_, ok := err.(*cache.ErrCacheNotStarted)
	return ok
}

// Add creates a new HostPathProvisioner Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager) error {
	return add(mgr, newReconciler(mgr))
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager) reconcile.Reconciler {
	mgrScheme := mgr.GetScheme()
	if err := hostpathprovisionerv1.AddToScheme(mgr.GetScheme()); err != nil {
		panic(err)
	}

	if err := promv1.AddToScheme(mgr.GetScheme()); err != nil {
		panic(err)
	}

	return &ReconcileHostPathProvisioner{
		client:   mgr.GetClient(),
		scheme:   mgrScheme,
		recorder: mgr.GetEventRecorderFor("operator-controller"),
		Log:      log,
	}
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// Create a new controller
	c, err := controller.New("hostpathprovisioner-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// mapFn will be used to map reconcile requests to the HPP for resources that don't have an ownerRef
	mapFn := handler.MapFunc(func(_ context.Context, o client.Object) []reconcile.Request {
		if val, ok := o.GetLabels()["k8s-app"]; ok && val == MultiPurposeHostPathProvisionerName {
			hppList, err := getHppList(mgr.GetClient())
			if err != nil {
				log.Error(err, "Error getting HPPs")
				return nil
			}
			if size := len(hppList.Items); size != 1 {
				log.Info("There should be exactly one HPP instance")
				return nil
			}

			return []reconcile.Request{
				{
					NamespacedName: types.NamespacedName{
						Name: hppList.Items[0].Name,
					},
				},
			}
		}
		return nil
	})

	// handleAPIServer will be used to handle APIServer Watch triggering
	handleAPIServer := handler.TypedMapFunc[*ocpconfigv1.APIServer, reconcile.Request](handleAPIServerFunc)

	// Watch for changes to primary resource HostPathProvisioner
	err = c.Watch(source.Kind(
		mgr.GetCache(),
		&hostpathprovisionerv1.HostPathProvisioner{},
		&handler.TypedEnqueueRequestForObject[*hostpathprovisionerv1.HostPathProvisioner]{}))
	if err != nil {
		return err
	}

	err = c.Watch(source.Kind(
		mgr.GetCache(),
		&appsv1.DaemonSet{},
		handler.TypedEnqueueRequestForOwner[*appsv1.DaemonSet](
			mgr.GetScheme(),
			mgr.GetRESTMapper(),
			&hostpathprovisionerv1.HostPathProvisioner{},
			handler.OnlyControllerOwner())))
	if err != nil {
		return err
	}

	err = c.Watch(source.Kind(
		mgr.GetCache(),
		&appsv1.Deployment{},
		handler.TypedEnqueueRequestForOwner[*appsv1.Deployment](
			mgr.GetScheme(),
			mgr.GetRESTMapper(),
			&hostpathprovisionerv1.HostPathProvisioner{},
			handler.OnlyControllerOwner())))
	if err != nil {
		return err
	}

	err = c.Watch(source.Kind(
		mgr.GetCache(),
		&corev1.ServiceAccount{},
		handler.TypedEnqueueRequestForOwner[*corev1.ServiceAccount](
			mgr.GetScheme(),
			mgr.GetRESTMapper(),
			&hostpathprovisionerv1.HostPathProvisioner{},
			handler.OnlyControllerOwner())))
	if err != nil {
		return err
	}

	err = c.Watch(source.Kind(
		mgr.GetCache(),
		&rbacv1.RoleBinding{},
		handler.TypedEnqueueRequestForOwner[*rbacv1.RoleBinding](
			mgr.GetScheme(),
			mgr.GetRESTMapper(),
			&hostpathprovisionerv1.HostPathProvisioner{},
			handler.OnlyControllerOwner())))
	if err != nil {
		return err
	}

	err = c.Watch(source.Kind(
		mgr.GetCache(),
		&rbacv1.Role{},
		handler.TypedEnqueueRequestForOwner[*rbacv1.Role](
			mgr.GetScheme(),
			mgr.GetRESTMapper(),
			&hostpathprovisionerv1.HostPathProvisioner{},
			handler.OnlyControllerOwner())))
	if err != nil {
		return err
	}

	if err := c.Watch(source.Kind(
		mgr.GetCache(),
		&storagev1.CSIDriver{},
		handler.TypedEnqueueRequestsFromMapFunc[*storagev1.CSIDriver, reconcile.Request](handler.TypedMapFunc[*storagev1.CSIDriver, reconcile.Request](func(ctx context.Context, o *storagev1.CSIDriver) []reconcile.Request {
			return mapFn(ctx, o)
		})))); err != nil {
		return err
	}

	if err := c.Watch(source.Kind(
		mgr.GetCache(),
		&rbacv1.ClusterRoleBinding{},
		handler.TypedEnqueueRequestsFromMapFunc[*rbacv1.ClusterRoleBinding, reconcile.Request](handler.TypedMapFunc[*rbacv1.ClusterRoleBinding, reconcile.Request](func(ctx context.Context, o *rbacv1.ClusterRoleBinding) []reconcile.Request {
			return mapFn(ctx, o)
		})))); err != nil {
		return err
	}

	if err := c.Watch(source.Kind(
		mgr.GetCache(),
		&rbacv1.ClusterRole{},
		handler.TypedEnqueueRequestsFromMapFunc[*rbacv1.ClusterRole, reconcile.Request](handler.TypedMapFunc[*rbacv1.ClusterRole, reconcile.Request](func(ctx context.Context, o *rbacv1.ClusterRole) []reconcile.Request {
			return mapFn(ctx, o)
		})))); err != nil {
		return err
	}

	if err := c.Watch(source.Kind(
		mgr.GetCache(),
		&rbacv1.Role{},
		handler.TypedEnqueueRequestsFromMapFunc[*rbacv1.Role, reconcile.Request](handler.TypedMapFunc[*rbacv1.Role, reconcile.Request](func(ctx context.Context, o *rbacv1.Role) []reconcile.Request {
			return mapFn(ctx, o)
		})))); err != nil {
		return err
	}
	if err := c.Watch(source.Kind(
		mgr.GetCache(),
		&rbacv1.RoleBinding{},
		handler.TypedEnqueueRequestsFromMapFunc[*rbacv1.RoleBinding, reconcile.Request](handler.TypedMapFunc[*rbacv1.RoleBinding, reconcile.Request](func(ctx context.Context, o *rbacv1.RoleBinding) []reconcile.Request {
			return mapFn(ctx, o)
		})))); err != nil {
		return err
	}
	if err := c.Watch(source.Kind(
		mgr.GetCache(),
		&corev1.Service{},
		handler.TypedEnqueueRequestsFromMapFunc[*corev1.Service, reconcile.Request](handler.TypedMapFunc[*corev1.Service, reconcile.Request](func(ctx context.Context, o *corev1.Service) []reconcile.Request {
			return mapFn(ctx, o)
		})))); err != nil {
		return err
	}

	if used, err := r.(*ReconcileHostPathProvisioner).checkSCCUsed(); used || isErrCacheNotStarted(err) {
		if err := c.Watch(source.Kind(
			mgr.GetCache(),
			&secv1.SecurityContextConstraints{},
			handler.TypedEnqueueRequestsFromMapFunc[*secv1.SecurityContextConstraints, reconcile.Request](handler.TypedMapFunc[*secv1.SecurityContextConstraints, reconcile.Request](func(ctx context.Context, o *secv1.SecurityContextConstraints) []reconcile.Request {
				return mapFn(ctx, o)
			})))); err != nil {
			if meta.IsNoMatchError(err) {
				log.Info("Not watching SecurityContextConstraints")
				return nil
			}
			return err
		}
		if err := c.Watch(source.Kind(
			mgr.GetCache(),
			&ocpconfigv1.APIServer{},
			handler.TypedEnqueueRequestsFromMapFunc[*ocpconfigv1.APIServer](handleAPIServer))); err != nil {
			if meta.IsNoMatchError(err) {
				log.Info("Not watching APIServer")
				return nil
			}
			return err
		}
	}

	if used, err := r.(*ReconcileHostPathProvisioner).checkPrometheusUsed(); used || isErrCacheNotStarted(err) {
		if err := c.Watch(source.Kind(
			mgr.GetCache(),
			&promv1.PrometheusRule{},
			handler.TypedEnqueueRequestsFromMapFunc[*promv1.PrometheusRule, reconcile.Request](handler.TypedMapFunc[*promv1.PrometheusRule, reconcile.Request](func(ctx context.Context, o *promv1.PrometheusRule) []reconcile.Request {
				return mapFn(ctx, o)
			})))); err != nil {
			if meta.IsNoMatchError(err) {
				log.Info("Not watching PrometheusRules")
				return nil
			}
			return err
		}
		if err := c.Watch(source.Kind(
			mgr.GetCache(),
			&promv1.ServiceMonitor{},
			handler.TypedEnqueueRequestsFromMapFunc[*promv1.ServiceMonitor, reconcile.Request](handler.TypedMapFunc[*promv1.ServiceMonitor, reconcile.Request](func(ctx context.Context, o *promv1.ServiceMonitor) []reconcile.Request {
				return mapFn(ctx, o)
			})))); err != nil {
			if meta.IsNoMatchError(err) {
				log.Info("Not watching ServiceMonitors")
				return nil
			}
			return err
		}
	}

	return nil
}

// blank assignment to verify that ReconcileHostPathProvisioner implements reconcile.Reconciler
var _ reconcile.Reconciler = &ReconcileHostPathProvisioner{}

// ReconcileHostPathProvisioner reconciles a HostPathProvisioner object
type ReconcileHostPathProvisioner struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver
	client   client.Client
	scheme   *runtime.Scheme
	recorder record.EventRecorder
	Log      logr.Logger
}

// Reconcile reads that state of the cluster for a HostPathProvisioner object and makes changes based on the state read
// and what is in the HostPathProvisioner.Spec
// The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r *ReconcileHostPathProvisioner) Reconcile(context context.Context, request reconcile.Request) (reconcile.Result, error) {
	reqLogger := r.Log.WithValues("Request.Namespace", request.Namespace, "Request.Name", request.Name)
	reqLogger.V(3).Info("Reconciling HostPathProvisioner")

	// Checks that only a single HPP instance exists
	hppList, err := getHppList(r.client)
	if err != nil {
		reqLogger.Error(err, "Error getting HPPs")
		return reconcile.Result{}, err
	}
	if size := len(hppList.Items); size > 1 {
		err := fmt.Errorf("there should be a single hostpath provisioner, %d items found", size)
		reqLogger.Error(err, "Multiple HPPs detected")
		return reconcile.Result{}, err
	}

	versionString, err := version.VersionStringFunc()
	if err != nil {
		return reconcile.Result{}, err
	}

	// Fetch the HostPathProvisioner instance
	cr := &hostpathprovisionerv1.HostPathProvisioner{}
	err = r.client.Get(context, request.NamespacedName, cr)
	if err != nil {
		if errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	}
	if r.isLegacy(cr) {
		reqLogger.Info("Detected legacy CR, Reconciling CSI and legacy controller plugin")
	} else {
		reqLogger.Info("Detected CSI CR, reconciling CSI only")
	}

	// Ready metric so we can alert whenever we are not ready for a while
	if IsHppAvailable(cr) {
		metrics.SetReadyGaugeValue(1)
	} else if !IsHppProgressing(cr) {
		// Not an issue if progress is still ongoing
		metrics.SetReadyGaugeValue(0)
	}

	namespace := watchNamespaceFunc()

	if cr.GetDeletionTimestamp() != nil {
		if err := r.cleanDeployments(reqLogger, cr, namespace); err != nil {
			return reconcile.Result{}, err
		}
		if res, err := r.reconcileCleanup(reqLogger, cr, namespace, 0); err != nil || res.RequeueAfter == time.Second {
			return res, err
		}
		reqLogger.Info("Deleting SecurityContextConstraint", "SecurityContextConstraints", MultiPurposeHostPathProvisionerName)
		if err := r.deleteSCC(MultiPurposeHostPathProvisionerName); err != nil {
			reqLogger.Error(err, "Unable to delete SecurityContextConstraints")
			// TODO, should we return and in essence keep retrying, and thus never be able to delete the CR if deleting the SCC fails, or
			// should be not return and allow the CR to be deleted but without deleting the SCC if that fails.
			return reconcile.Result{}, err
		}
		if err := r.deleteSCC(fmt.Sprintf("%s-csi", MultiPurposeHostPathProvisionerName)); err != nil {
			reqLogger.Error(err, "Unable to delete CSI SecurityContextConstraints")
			// TODO, should we return and in essence keep retrying, and thus never be able to delete the CR if deleting the SCC fails, or
			// should be not return and allow the CR to be deleted but without deleting the SCC if that fails.
			return reconcile.Result{}, err
		}
		if err := r.deletePrometheusResources(namespace); err != nil {
			reqLogger.Error(err, "Unable to delete Prometheus Infra (PrometheusRule, ServiceMonitor, RBAC)")
			return reconcile.Result{}, err
		}
		if res, err := r.deleteAllRbac(reqLogger, namespace); err != nil {
			return res, err
		}
		reqLogger.Info("Deleting CSIDriver", "CSIDriver", MultiPurposeHostPathProvisionerName)
		if err := r.deleteCSIDriver(); err != nil {
			reqLogger.Error(err, "Unable to delete CSIDriver")
			return reconcile.Result{}, err
		}
		RemoveFinalizer(cr, hppFinalizer)

		// Update CR
		err = r.client.Update(context, cr)
		if err != nil {
			reqLogger.Error(err, "Unable to remove finalizer from CR")
			return reconcile.Result{}, err
		}
		return reconcile.Result{}, nil
	}

	currentCopy := cr.DeepCopy()
	// Add finalizer for this CR
	if err := r.addFinalizer(reqLogger, cr); err != nil {
		return reconcile.Result{}, err
	}

	cr.Status.OperatorVersion = versionString
	cr.Status.TargetVersion = versionString
	canUpgrade, err := canUpgrade(cr.Status.ObservedVersion, versionString)
	if err != nil {
		// Downgrading not supported
		return reconcile.Result{}, err
	}
	if r.isDeploying(cr) {
		//New install, mark deploying.
		MarkCrDeploying(cr, deployStarted, deployStartedMessage)
		r.recorder.Event(cr, corev1.EventTypeNormal, deployStarted, deployStartedMessage)
		err = r.client.Update(context, cr)
		if err != nil {
			reqLogger.Info("Marked deploying failed", "Error", err.Error())
			// Error updating the object - requeue the request.
			return reconcile.Result{}, err
		}
		reqLogger.Info("Started deploying")
	}

	if canUpgrade && r.isUpgrading(cr) {
		MarkCrUpgradeHealingDegraded(cr, upgradeStarted, fmt.Sprintf("Started upgrade to version %s", cr.Status.TargetVersion))
		r.recorder.Event(cr, corev1.EventTypeWarning, upgradeStarted, fmt.Sprintf("Started upgrade to version %s", cr.Status.TargetVersion))
		// Mark Observed version to blank, so we get to the reconcile upgrade section.
		err = r.client.Update(context, cr)
		if err != nil {
			// Error updating the object - requeue the request.
			return reconcile.Result{}, err
		}
		reqLogger.Info("Started upgrading")
	}

	res, err := r.reconcileUpdate(reqLogger, cr, namespace)
	if err == nil {
		res, err = r.reconcileStatus(context, reqLogger, cr, namespace, versionString)
	} else {
		MarkCrFailedHealing(cr, reconcileFailed, fmt.Sprintf("Unable to successfully reconcile: %v", err))
		r.recorder.Event(cr, corev1.EventTypeWarning, reconcileFailed, fmt.Sprintf("Unable to successfully reconcile: %v", err))
	}

	r.ignoreHeartBeatTimestamp(currentCopy, cr)
	if !reflect.DeepEqual(currentCopy, cr) {
		logJSONDiff(reqLogger, currentCopy, cr)
		updateErr := r.client.Update(context, cr)
		if updateErr != nil {
			r.Log.Error(err, "Unable to successfully reconcile")
			err = updateErr
		}
	}
	return res, err
}

func (r *ReconcileHostPathProvisioner) reconcileCleanup(reqLogger logr.Logger, cr *hostpathprovisionerv1.HostPathProvisioner, namespace string, deploymentCount int) (reconcile.Result, error) {
	spDeployments, err := r.currentStoragePoolDeployments(cr, namespace)
	if err != nil {
		return reconcile.Result{}, err
	}
	reqLogger.Info("Number of storage pool deployments still active", "count", len(spDeployments))
	cleanupFinished, err := r.hasCleanUpFinished(namespace)
	if err != nil {
		return reconcile.Result{}, err
	}
	if len(spDeployments) == deploymentCount && cleanupFinished {
		if err := r.removeCleanUpJobs(reqLogger, namespace); err != nil {
			return reconcile.Result{}, err
		}
	} else {
		return reconcile.Result{RequeueAfter: time.Second}, nil
	}
	return reconcile.Result{}, nil
}

func (r *ReconcileHostPathProvisioner) ignoreHeartBeatTimestamp(currentCopy, cr *hostpathprovisionerv1.HostPathProvisioner) {
	for i, condition := range currentCopy.Status.Conditions {
		crCond := conditions.FindStatusCondition(cr.Status.Conditions, condition.Type)
		if crCond.Message == condition.Message && crCond.Reason == condition.Reason && crCond.Status == condition.Status {
			currentCopy.Status.Conditions[i].LastHeartbeatTime = crCond.LastHeartbeatTime
		}
	}
}

func (r *ReconcileHostPathProvisioner) isLegacy(cr *hostpathprovisionerv1.HostPathProvisioner) bool {
	return cr.Spec.PathConfig != nil
}

func (r *ReconcileHostPathProvisioner) reconcileStatus(_ context.Context, reqLogger logr.Logger, cr *hostpathprovisionerv1.HostPathProvisioner, namespace, versionString string) (reconcile.Result, error) {
	// Check if all requested pods are available.
	degraded, err := r.checkDegraded(reqLogger, cr, namespace)
	if err != nil {
		return reconcile.Result{}, err
	}
	if err := r.reconcileStoragePoolStatus(reqLogger, cr, namespace); err != nil {
		MarkCrFailedHealing(cr, "StoragePoolNotReady", err.Error())
		return reconcile.Result{}, err
	}
	if !degraded && cr.Status.ObservedVersion != versionString {
		cr.Status.ObservedVersion = versionString
	}
	return reconcile.Result{}, nil
}

func (r *ReconcileHostPathProvisioner) deleteAllRbac(reqLogger logr.Logger, namespace string) (reconcile.Result, error) {
	for _, name := range []string{ProvisionerServiceAccountName, ProvisionerServiceAccountNameCsi, MultiPurposeHostPathProvisionerName} {
		reqLogger.Info("Deleting ClusterRoleBinding", "ClusterRoleBinding", name)
		if err := r.deleteClusterRoleBindingObject(name); err != nil {
			reqLogger.Error(err, "Unable to delete ClusterRoleBinding")
			return reconcile.Result{}, err
		}
		reqLogger.Info("Deleting ClusterRole", "ClusterRole", name)
		if err := r.deleteClusterRoleObject(name); err != nil {
			reqLogger.Error(err, "Unable to delete ClusterRole")
			return reconcile.Result{}, err
		}
		reqLogger.Info("Deleting RoleBinding", "ClusterRoleBinding", name)
		if err := r.deleteRoleBindingObject(name, namespace); err != nil {
			reqLogger.Error(err, "Unable to delete RoleBinding")
			return reconcile.Result{}, err
		}
		reqLogger.Info("Deleting Role", "ClusterRole", name)
		if err := r.deleteRoleObject(name, namespace); err != nil {
			reqLogger.Error(err, "Unable to delete Role")
			return reconcile.Result{}, err
		}
	}
	return reconcile.Result{}, nil
}

func canUpgrade(current, target string) (bool, error) {
	if current == "" {
		// Can't upgrade if no current is set
		return false, nil
	}

	if target == current {
		return false, nil
	}

	result := true
	// semver doesn't like the 'v' prefix
	targetSemver, errTarget := version.GetVersionFromString(target)
	currentSemver, errCurrent := version.GetVersionFromString(current)

	if errTarget == nil && errCurrent == nil {
		if targetSemver.Compare(*currentSemver) < 0 {
			err := fmt.Errorf("operator downgraded from %s to %s, will not reconcile", currentSemver.String(), targetSemver.String())
			return false, err
		} else if targetSemver.Compare(*currentSemver) == 0 {
			result = false
		}
	}
	return result, nil
}

func (r *ReconcileHostPathProvisioner) reconcileUpdate(reqLogger logr.Logger, cr *hostpathprovisionerv1.HostPathProvisioner, namespace string) (reconcile.Result, error) {
	// Reconcile the objects this operator manages.
	res, err := r.reconcileDaemonSet(reqLogger, cr, namespace)
	if err != nil {
		reqLogger.Error(err, "unable to create DaemonSet")
		return res, err
	}
	// Reconcile storage pools
	res, err = r.reconcileStoragePools(reqLogger, cr, namespace)
	if err != nil {
		reqLogger.Error(err, "unable to configure storage pools")
		return res, err
	}
	res, err = r.reconcileServiceAccount(reqLogger, cr, namespace)
	if err != nil {
		reqLogger.Error(err, "unable to create ServiceAccount")
		return res, err
	}
	res, err = r.reconcileClusterRole(reqLogger, cr)
	if err != nil {
		reqLogger.Error(err, "unable to create ClusterRole")
		return res, err
	}
	res, err = r.reconcileClusterRoleBinding(reqLogger, cr, namespace)
	if err != nil {
		reqLogger.Error(err, "unable to create ClusterRoleBinding")
		return res, err
	}
	res, err = r.reconcileRole(reqLogger, cr, namespace)
	if err != nil {
		reqLogger.Error(err, "unable to create Role")
		return res, err
	}
	res, err = r.reconcileRoleBinding(reqLogger, cr, namespace)
	if err != nil {
		reqLogger.Error(err, "unable to create RoleBinding")
		return res, err
	}
	res, err = r.reconcileCSIDriver(reqLogger, cr)
	if err != nil {
		reqLogger.Error(err, "unable to create CSIDriver")
		return res, err
	}
	res, err = r.reconcileSecurityContextConstraints(reqLogger, cr, namespace)
	if err != nil {
		reqLogger.Error(err, "unable to create SecurityContextConstraints")
		return res, err
	}
	res, err = r.reconcilePrometheusInfra(reqLogger, cr, namespace)
	if err != nil {
		reqLogger.Error(err, "unable to create Prometheus Infra (PrometheusRule, ServiceMonitor, RBAC)")
		return res, err
	}
	daemonSet := &appsv1.DaemonSet{}
	if r.isLegacy(cr) {
		if err := r.client.Get(context.TODO(), types.NamespacedName{Name: MultiPurposeHostPathProvisionerName, Namespace: namespace}, daemonSet); err != nil {
			return reconcile.Result{}, err
		}
	}
	daemonSetCsi := &appsv1.DaemonSet{}
	if err := r.client.Get(context.TODO(), types.NamespacedName{Name: fmt.Sprintf("%s-csi", MultiPurposeHostPathProvisionerName), Namespace: namespace}, daemonSetCsi); err != nil {
		return reconcile.Result{}, err
	}
	if (!r.isLegacy(cr) || checkDaemonSetReady(daemonSet)) && checkDaemonSetReady(daemonSetCsi) {
		MarkCrHealthyMessage(cr, "Complete", "Application Available")
		r.recorder.Event(cr, corev1.EventTypeNormal, provisionerHealthy, provisionerHealthyMessage)
	}
	if res, err := r.reconcileCleanup(reqLogger, cr, namespace, int(daemonSetCsi.Status.DesiredNumberScheduled)); err != nil || res.RequeueAfter == time.Second {
		return res, err
	}

	return res, nil
}

func (r *ReconcileHostPathProvisioner) checkDegraded(logger logr.Logger, cr *hostpathprovisionerv1.HostPathProvisioner, namespace string) (bool, error) {
	degraded := false

	daemonSet := &appsv1.DaemonSet{}
	if r.isLegacy(cr) {
		err := r.client.Get(context.TODO(), types.NamespacedName{Name: MultiPurposeHostPathProvisionerName, Namespace: namespace}, daemonSet)
		if err != nil {
			return true, err
		}
	}
	daemonSetCsi := &appsv1.DaemonSet{}
	err := r.client.Get(context.TODO(), types.NamespacedName{Name: fmt.Sprintf("%s-csi", MultiPurposeHostPathProvisionerName), Namespace: namespace}, daemonSetCsi)
	if err != nil {
		return true, err
	}

	if !((!r.isLegacy(cr) || checkDaemonSetReady(daemonSet)) && checkDaemonSetReady(daemonSetCsi)) {
		degraded = true
	}

	logger.V(3).Info("Degraded check", "Degraded", degraded)

	if degraded && !r.isDeploying(cr) {
		MarkCrFailed(cr, "Degraded", "CR is deployed but DaemonSets are not ready")
	}

	logger.V(3).Info("Finished degraded check", "conditions", cr.Status.Conditions)
	return degraded, nil
}

func checkDaemonSetReady(daemonSet *appsv1.DaemonSet) bool {
	return checkApplicationAvailable(daemonSet) && daemonSet.Status.NumberReady >= daemonSet.Status.DesiredNumberScheduled
}

func checkApplicationAvailable(daemonSet *appsv1.DaemonSet) bool {
	return daemonSet.Status.NumberReady > 0
}

func (r *ReconcileHostPathProvisioner) addFinalizer(reqLogger logr.Logger, obj client.Object) error {
	if obj.GetDeletionTimestamp() == nil {
		currentFinalizers := obj.GetFinalizers()
		reqLogger.V(3).Info("Adding deletion Finalizer")
		AddFinalizer(obj, hppFinalizer)
		// Only update if we modified the finalizers.
		if !reflect.DeepEqual(currentFinalizers, obj.GetFinalizers()) {
			// Update CR
			err := r.client.Update(context.TODO(), obj)
			if err != nil {
				reqLogger.Error(err, "Failed to update cr with finalizer")
				return err
			}
		}
	}
	return nil
}

func (r *ReconcileHostPathProvisioner) isFeatureGateEnabled(feature string, cr *hostpathprovisionerv1.HostPathProvisioner) bool {
	for _, featuregate := range cr.Spec.FeatureGates {
		if featuregate == feature {
			return true
		}
	}
	return false
}

// This function returns the list of HPP instances in the cluster and an error otherwise
func getHppList(c client.Client) (*hostpathprovisionerv1.HostPathProvisionerList, error) {
	hppList := &hostpathprovisionerv1.HostPathProvisionerList{}

	if err := c.List(context.TODO(), hppList, &client.ListOptions{}); err != nil {
		return nil, err
	}

	return hppList, nil
}

// AddFinalizer adds a finalizer to a resource
func AddFinalizer(obj metav1.Object, name string) {
	if HasFinalizer(obj, name) {
		return
	}

	obj.SetFinalizers(append(obj.GetFinalizers(), name))
}

// RemoveFinalizer removes a finalizer from a resource
func RemoveFinalizer(obj metav1.Object, name string) {
	if !HasFinalizer(obj, name) {
		return
	}

	var finalizers []string
	for _, f := range obj.GetFinalizers() {
		if f != name {
			finalizers = append(finalizers, f)
		}
	}

	obj.SetFinalizers(finalizers)
}

// HasFinalizer returns true if a resource has a specific finalizer
func HasFinalizer(object metav1.Object, value string) bool {
	for _, f := range object.GetFinalizers() {
		if f == value {
			return true
		}
	}
	return false
}

func handleAPIServerFunc(_ context.Context, apiServer *ocpconfigv1.APIServer) []reconcile.Request {
	cipherNames, minTypedTLSVersion := cryptopolicy.SelectCipherSuitesAndMinTLSVersion(apiServer.Spec.TLSSecurityProfile)
	if err := os.Setenv("TLS_CIPHERS", strings.Join(cipherNames, ",")); err != nil {
		log.Error(err, "Error setting environment variable TLS_CIPHERS")
	}
	if err := os.Setenv("TLS_MIN_VERSION", string(minTypedTLSVersion)); err != nil {
		log.Error(err, "Error setting environment variable TLS_MIN_VERSION")
	}
	return nil
}
