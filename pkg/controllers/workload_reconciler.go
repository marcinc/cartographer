// Copyright 2021 VMware
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

//go:generate go run -modfile ../../hack/tools/go.mod github.com/maxbrunsfeld/counterfeiter/v6 -generate

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/cluster-api/controllers/external"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/vmware-tanzu/cartographer/pkg/apis/v1alpha1"
	"github.com/vmware-tanzu/cartographer/pkg/conditions"
	"github.com/vmware-tanzu/cartographer/pkg/enqueuer"
	cerrors "github.com/vmware-tanzu/cartographer/pkg/errors"
	"github.com/vmware-tanzu/cartographer/pkg/logger"
	"github.com/vmware-tanzu/cartographer/pkg/mapper"
	realizerclient "github.com/vmware-tanzu/cartographer/pkg/realizer/client"
	realizer "github.com/vmware-tanzu/cartographer/pkg/realizer/workload"
	"github.com/vmware-tanzu/cartographer/pkg/repository"
	"github.com/vmware-tanzu/cartographer/pkg/resources"
	"github.com/vmware-tanzu/cartographer/pkg/tracker/dependency"
	"github.com/vmware-tanzu/cartographer/pkg/tracker/stamped"
	"github.com/vmware-tanzu/cartographer/pkg/utils"
)

type WorkloadReconciler struct {
	Repo                    repository.Repository
	ConditionManagerBuilder conditions.ConditionManagerBuilder
	ResourceRealizerBuilder realizer.ResourceRealizerBuilder
	Realizer                realizer.Realizer
	conditionManager        conditions.ConditionManager
	StampedTracker          stamped.StampedTracker
	DependencyTracker       dependency.DependencyTracker
}

func (r *WorkloadReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logr.FromContextOrDiscard(ctx)
	log.Info("started")
	defer log.Info("finished")

	log = log.WithValues("workload", req.NamespacedName)
	ctx = logr.NewContext(ctx, log)

	workload, err := r.Repo.GetWorkload(ctx, req.Name, req.Namespace)
	if err != nil {
		log.Error(err, "failed to get workload")
		return ctrl.Result{}, fmt.Errorf("failed to get workload [%s]: %w", req.NamespacedName, err)
	}

	if workload == nil {
		log.Info("workload no longer exists")
		r.DependencyTracker.ClearTracked(types.NamespacedName{
			Namespace: req.Namespace,
			Name:      req.Name,
		})

		return ctrl.Result{}, nil
	}

	r.conditionManager = r.ConditionManagerBuilder(v1alpha1.OwnerReady, workload.Status.Conditions)

	resourceStatuses := resources.NewResourceStatuses(workload.Status.Resources)

	supplyChain, err := r.getSupplyChainsForWorkload(ctx, workload)
	if err != nil {
		return r.completeReconciliation(ctx, workload, resourceStatuses, err)
	}

	log = log.WithValues("supply chain", supplyChain.Name)
	ctx = logr.NewContext(ctx, log)

	supplyChainGVK, err := utils.GetObjectGVK(supplyChain, r.Repo.GetScheme())
	if err != nil {
		log.Error(err, "failed to get object gvk for supply chain")
		return r.completeReconciliation(ctx, workload, resourceStatuses, cerrors.NewUnhandledError(
			fmt.Errorf("failed to get object gvk for supply chain [%s]: %w", supplyChain.Name, err)),
		)
	}

	workload.Status.SupplyChainRef.Kind = supplyChainGVK.Kind
	workload.Status.SupplyChainRef.Name = supplyChain.Name

	if !r.isSupplyChainReady(supplyChain) {
		r.conditionManager.AddPositive(conditions.MissingReadyInSupplyChainCondition(getSupplyChainReadyCondition(supplyChain)))
		log.Info("supply chain is not in ready state")
		return r.completeReconciliation(ctx, workload, resourceStatuses, fmt.Errorf("supply chain [%s] is not in ready state", supplyChain.Name))
	}
	r.conditionManager.AddPositive(conditions.SupplyChainReadyCondition())

	serviceAccountName, serviceAccountNS := getServiceAccountNameAndNamespaceForWorkload(workload, supplyChain)

	secret, err := r.Repo.GetServiceAccountSecret(ctx, serviceAccountName, serviceAccountNS)
	if err != nil {
		r.conditionManager.AddPositive(conditions.ServiceAccountSecretNotFoundCondition(err))
		log.Info("failed to get service account secret", "service account", fmt.Sprintf("%s/%s", serviceAccountNS, serviceAccountName))
		return r.completeReconciliation(ctx, workload, resourceStatuses, fmt.Errorf("failed to get service account secret [%s]: %w", fmt.Sprintf("%s/%s", serviceAccountNS, serviceAccountName), err))
	}

	resourceRealizer, err := r.ResourceRealizerBuilder(secret, workload, r.Repo, supplyChain.Spec.Params)
	if err != nil {
		r.conditionManager.AddPositive(conditions.ResourceRealizerBuilderErrorCondition(err))
		log.Error(err, "failed to build resource realizer")
		return r.completeReconciliation(ctx, workload, resourceStatuses, cerrors.NewUnhandledError(
			fmt.Errorf("failed to build resource realizer: %w", err)))
	}

	err = r.Realizer.Realize(ctx, resourceRealizer, supplyChain, resourceStatuses)

	if err != nil {
		// TODO: Use interface for differences
		conditions.AddConditionForResourceSubmitted(&r.conditionManager, true, err)
	} else {
		r.conditionManager.AddPositive(conditions.ResourcesSubmittedCondition(true))
	}

	if err != nil {
		log.V(logger.DEBUG).Info("failed to realize")
		if cerrors.IsUnhandledErrorType(err) {
			err = cerrors.NewUnhandledError(err)
		}
	} else {
		if log.V(logger.DEBUG).Enabled() {
			for _, resource := range resourceStatuses.GetCurrent() {
				log.V(logger.DEBUG).Info("realized object",
					"object", resource.StampedRef)
			}
		}
	}

	r.trackDependencies(workload, resourceStatuses.GetCurrent(), serviceAccountName, serviceAccountNS)

	cleanupErr := r.cleanupOrphanedObjects(ctx, workload.Status.Resources, resourceStatuses.GetCurrent())
	if cleanupErr != nil {
		log.Error(cleanupErr, "failed to cleanup orphaned objects")
	}

	var trackingError error
	for _, resource := range resourceStatuses.GetCurrent() {
		if resource.StampedRef == nil {
			continue
		}
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(resource.StampedRef.GroupVersionKind())

		trackingError = r.StampedTracker.Watch(log, obj, &handler.EnqueueRequestForOwner{OwnerType: &v1alpha1.Workload{}})
		if trackingError != nil {
			log.Error(err, "failed to add informer for object",
				"object", resource.StampedRef)
			err = cerrors.NewUnhandledError(trackingError)
		} else {
			log.V(logger.DEBUG).Info("added informer for object",
				"object", resource.StampedRef)
		}
	}

	return r.completeReconciliation(ctx, workload, resourceStatuses, err)
}

func (r *WorkloadReconciler) completeReconciliation(ctx context.Context, workload *v1alpha1.Workload, resourceStatuses resources.ResourceStatuses, err error) (ctrl.Result, error) {
	log := logr.FromContextOrDiscard(ctx)
	var changed bool
	workload.Status.Conditions, changed = r.conditionManager.Finalize()
	var updateErr error
	// TODO: Implement IsChanged()
	if changed || (workload.Status.ObservedGeneration != workload.Generation) || resourceStatuses.IsChanged() {
		workload.Status.Resources = resourceStatuses.GetCurrent()
		workload.Status.ObservedGeneration = workload.Generation
		updateErr = r.Repo.StatusUpdate(ctx, workload)
		if updateErr != nil {
			log.Error(err, "failed to update status for workload")
			return ctrl.Result{}, fmt.Errorf("failed to update status for workload: %w", updateErr)
		}
	}

	if err != nil {
		if cerrors.IsUnhandledError(err) {
			log.Error(err, "unhandled error reconciling workload")
			return ctrl.Result{}, err
		}
		log.Info("handled error reconciling workload", "handled error", err)
	}

	return ctrl.Result{}, nil
}

func (r *WorkloadReconciler) isSupplyChainReady(supplyChain *v1alpha1.ClusterSupplyChain) bool {
	supplyChainReadyCondition := getSupplyChainReadyCondition(supplyChain)
	return supplyChainReadyCondition.Status == "True"
}

func getSupplyChainReadyCondition(supplyChain *v1alpha1.ClusterSupplyChain) metav1.Condition {
	for _, condition := range supplyChain.Status.Conditions {
		if condition.Type == "Ready" {
			return condition
		}
	}
	return metav1.Condition{}
}

func (r *WorkloadReconciler) getSupplyChainsForWorkload(ctx context.Context, workload *v1alpha1.Workload) (*v1alpha1.ClusterSupplyChain, error) {
	log := logr.FromContextOrDiscard(ctx)
	if len(workload.Labels) == 0 {
		r.conditionManager.AddPositive(conditions.WorkloadMissingLabelsCondition())
		log.Info("workload is missing required labels")
		return nil, fmt.Errorf("workload [%s/%s] is missing required labels",
			workload.Namespace, workload.Name)
	}

	supplyChains, err := r.Repo.GetSupplyChainsForWorkload(ctx, workload)
	if err != nil {
		log.Error(err, "failed to get supply chains for workload")
		return nil, cerrors.NewUnhandledError(fmt.Errorf("failed to get supply chains for workload [%s/%s]: %w",
			workload.Namespace, workload.Name, err))
	}

	if len(supplyChains) == 0 {
		r.conditionManager.AddPositive(conditions.SupplyChainNotFoundCondition(workload.Labels))
		log.Info("no supply chain found where full selector is satisfied by label",
			"labels", workload.Labels)
		return nil, fmt.Errorf("no supply chain [%s/%s] found where full selector is satisfied by labels: %v",
			workload.Namespace, workload.Name, workload.Labels)
	}

	if len(supplyChains) > 1 {
		r.conditionManager.AddPositive(conditions.TooManySupplyChainMatchesCondition())
		log.Info("more than one supply chain selected for workload",
			"supply chains", getSupplyChainNames(supplyChains))
		return nil, fmt.Errorf("more than one supply chain selected for workload [%s/%s]: %+v",
			workload.Namespace, workload.Name, getSupplyChainNames(supplyChains))
	}

	log.V(logger.DEBUG).Info("supply chain matched for workload", "supply chain", supplyChains[0].Name)
	return supplyChains[0], nil
}

func (r *WorkloadReconciler) trackDependencies(workload *v1alpha1.Workload, realizedResources []v1alpha1.ResourceStatus, serviceAccountName, serviceAccountNS string) {
	r.DependencyTracker.ClearTracked(types.NamespacedName{
		Namespace: workload.Namespace,
		Name:      workload.Name,
	})

	r.DependencyTracker.Track(dependency.Key{
		GroupKind: schema.GroupKind{
			Group: corev1.SchemeGroupVersion.Group,
			Kind:  rbacv1.ServiceAccountKind,
		},
		NamespacedName: types.NamespacedName{
			Namespace: serviceAccountNS,
			Name:      serviceAccountName,
		},
	}, types.NamespacedName{
		Namespace: workload.Namespace,
		Name:      workload.Name,
	})

	for _, resource := range realizedResources {
		if resource.TemplateRef == nil {
			continue
		}
		r.DependencyTracker.Track(
			dependency.Key{
				GroupKind: schema.GroupKind{
					Group: v1alpha1.SchemeGroupVersion.Group,
					Kind:  resource.TemplateRef.Kind,
				},
				NamespacedName: types.NamespacedName{
					Name: resource.TemplateRef.Name,
				},
			},
			types.NamespacedName{
				Namespace: workload.Namespace,
				Name:      workload.Name,
			},
		)
	}
}

func (r *WorkloadReconciler) cleanupOrphanedObjects(ctx context.Context, previousResources, realizedResources []v1alpha1.ResourceStatus) error {
	log := logr.FromContextOrDiscard(ctx)

	var orphanedObjs []*corev1.ObjectReference
	for _, prevResource := range previousResources {
		if prevResource.StampedRef == nil {
			continue
		}
		orphaned := true
		for _, realizedResource := range realizedResources {
			if realizedResource.StampedRef == nil {
				continue
			}
			if realizedResource.StampedRef.GroupVersionKind() == prevResource.StampedRef.GroupVersionKind() &&
				realizedResource.StampedRef.Namespace == prevResource.StampedRef.Namespace &&
				realizedResource.StampedRef.Name == prevResource.StampedRef.Name {
				orphaned = false
				break
			}
		}
		if orphaned {
			orphanedObjs = append(orphanedObjs, prevResource.StampedRef)
		}
	}

	for _, orphanedObj := range orphanedObjs {
		obj := &unstructured.Unstructured{}
		obj.SetNamespace(orphanedObj.Namespace)
		obj.SetName(orphanedObj.Name)
		obj.SetGroupVersionKind(orphanedObj.GroupVersionKind())

		log.V(logger.DEBUG).Info("deleting orphaned object", "object", orphanedObj)
		err := r.Repo.Delete(ctx, obj)
		if err != nil {
			return err
		}
	}

	return nil
}

func getSupplyChainNames(objs []*v1alpha1.ClusterSupplyChain) []string {
	var names []string
	for _, obj := range objs {
		names = append(names, obj.GetName())
	}

	return names
}

func getServiceAccountNameAndNamespaceForWorkload(workload *v1alpha1.Workload, supplyChain *v1alpha1.ClusterSupplyChain) (string, string) {
	serviceAccountName := "default"
	serviceAccountNS := workload.Namespace

	if workload.Spec.ServiceAccountName != "" {
		serviceAccountName = workload.Spec.ServiceAccountName
	} else if supplyChain.Spec.ServiceAccountRef.Name != "" {
		serviceAccountName = supplyChain.Spec.ServiceAccountRef.Name
		if supplyChain.Spec.ServiceAccountRef.Namespace != "" {
			serviceAccountNS = supplyChain.Spec.ServiceAccountRef.Namespace
		}
	}

	return serviceAccountName, serviceAccountNS
}

// TODO: kubebuilder:rbac
func (r *WorkloadReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Repo = repository.NewRepository(
		mgr.GetClient(),
		repository.NewCache(mgr.GetLogger().WithName("workload-repo-cache")),
	)
	r.ConditionManagerBuilder = conditions.NewConditionManager
	r.ResourceRealizerBuilder = realizer.NewResourceRealizerBuilder(
		repository.NewRepository, realizerclient.NewClientBuilder(mgr.GetConfig()),
		repository.NewCache(mgr.GetLogger().WithName("workload-stamping-repo-cache")),
	)
	r.Realizer = realizer.NewRealizer()
	r.DependencyTracker = dependency.NewDependencyTracker(
		2*utils.DefaultResyncTime,
		mgr.GetLogger().WithName("tracker-workload"),
	)

	builder := ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Workload{})

	m := mapper.Mapper{
		Client:  mgr.GetClient(),
		Logger:  mgr.GetLogger().WithName("workload"),
		Tracker: r.DependencyTracker,
	}

	watches := map[client.Object]handler.MapFunc{
		&v1alpha1.ClusterSupplyChain{}: m.ClusterSupplyChainToWorkloadRequests,
		&corev1.ServiceAccount{}:       m.ServiceAccountToWorkloadRequests,
		&rbacv1.Role{}:                 m.RoleToWorkloadRequests,
		&rbacv1.RoleBinding{}:          m.RoleBindingToWorkloadRequests,
		&rbacv1.ClusterRole{}:          m.ClusterRoleToWorkloadRequests,
		&rbacv1.ClusterRoleBinding{}:   m.ClusterRoleBindingToWorkloadRequests,
	}

	for kindType, mapFunc := range watches {
		builder = builder.Watches(
			&source.Kind{Type: kindType},
			handler.EnqueueRequestsFromMapFunc(mapFunc),
		)
	}

	for _, template := range v1alpha1.ValidSupplyChainTemplates {
		builder = builder.Watches(
			&source.Kind{Type: template},
			enqueuer.EnqueueTracked(template, r.DependencyTracker, mgr.GetScheme()),
		)
	}

	controller, err := builder.Build(r)
	if err != nil {
		return fmt.Errorf("failed to build controller for workload: %w", err)
	}
	r.StampedTracker = &external.ObjectTracker{Controller: controller}

	return nil
}
