/*
Copyright 2021 The RamenDR authors.

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
	"encoding/json"
	"fmt"
	"reflect"
	"sort"

	spokeClusterV1 "github.com/open-cluster-management/api/cluster/v1"
	ocmworkv1 "github.com/open-cluster-management/api/work/v1"
	plrv1 "github.com/open-cluster-management/multicloud-operators-placementrule/pkg/apis/apps/v1"
	"github.com/open-cluster-management/multicloud-operators-placementrule/pkg/utils"
	subv1 "github.com/open-cluster-management/multicloud-operators-subscription/pkg/apis/apps/v1"

	"github.com/go-logr/logr"
	errorswrapper "github.com/pkg/errors"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ramendrv1alpha1 "github.com/ramendr/ramen/api/v1alpha1"
)

const ManifestWorkNameFormat string = "%s-%s-vrg-mw"

// ApplicationVolumeReplicationReconciler reconciles a ApplicationVolumeReplication object
type ApplicationVolumeReplicationReconciler struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme
}

// SetupWithManager sets up the controller with the Manager.
func (r *ApplicationVolumeReplicationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&ramendrv1alpha1.ApplicationVolumeReplication{}).
		Complete(r)
}

//nolint:lll
// +kubebuilder:rbac:groups=ramendr.openshift.io,resources=applicationvolumereplications,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=ramendr.openshift.io,resources=applicationvolumereplications/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=ramendr.openshift.io,resources=applicationvolumereplications/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the ApplicationVolumeReplication object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.7.0/pkg/reconcile
func (r *ApplicationVolumeReplicationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := r.Log.WithValues("ApplicationVolumeReplication", req.NamespacedName)
	logger.Info("processing reconcile loop")

	defer logger.Info("exiting reconcile loop")

	avr := &ramendrv1alpha1.ApplicationVolumeReplication{}

	err := r.Client.Get(context.TODO(), req.NamespacedName, avr)
	if err != nil {
		if errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Return and don't requeue
			return ctrl.Result{}, nil
		}

		// requeue the request
		return ctrl.Result{}, errorswrapper.Wrap(err, "failed to get AVR object")
	}

	// TODO failover case will be handled starting from this commented line

	subscriptionList := &subv1.SubscriptionList{}
	listOptions := &client.ListOptions{Namespace: avr.Namespace}

	err = r.Client.List(context.TODO(), subscriptionList, listOptions)
	if err != nil {
		if !errors.IsNotFound(err) {
			logger.Error(err, "failed to find subscription list", "namespace", avr.Namespace)

			return ctrl.Result{Requeue: true}, nil
		}

		return ctrl.Result{}, errorswrapper.Wrap(err, "failed to list subscriptions")
	}

	placementDecisions, requeue := r.processSubscriptionList(subscriptionList)
	if len(placementDecisions) == 0 {
		logger.Error(nil, "no placement decisions were found", "namespace", avr.Namespace)

		return ctrl.Result{Requeue: true}, nil
	}

	if err := r.updateAVRStatus(ctx, avr, placementDecisions); err != nil {
		logger.Error(err, "failed to update status")

		return ctrl.Result{Requeue: true}, nil
	}

	logger.Info("completed manifestwork for subscriptions")

	return ctrl.Result{Requeue: requeue}, nil
}

func IsManifestInAppliedState(mw *ocmworkv1.ManifestWork) bool {
	applied := false
	degraded := false
	conditions := mw.Status.Conditions

	if len(conditions) > 0 {
		// get most recent conditions that have ConditionTrue status
		recentConditions := filterByConditionStatus(getMostRecentConditions(conditions), metav1.ConditionTrue)

		for _, condition := range recentConditions {
			if condition.Type == ocmworkv1.WorkApplied {
				applied = true
			} else if condition.Type == ocmworkv1.WorkDegraded {
				degraded = true
			}
		}

		// if most recent timestamp contains Applied and Degraded states, don't trust it's actually Applied
		if degraded {
			applied = false
		}
	}

	return applied
}

func filterByConditionStatus(conditions []metav1.Condition, status metav1.ConditionStatus) []metav1.Condition {
	filtered := make([]metav1.Condition, 0)

	for _, condition := range conditions {
		if condition.Status == status {
			filtered = append(filtered, condition)
		}
	}

	return filtered
}

// return Conditions with most recent timestamps only (allows duplicates)
func getMostRecentConditions(conditions []metav1.Condition) []metav1.Condition {
	recentConditions := make([]metav1.Condition, 0)

	// sort conditions by timestamp. Index 0 = most recent
	sort.Slice(conditions, func(a, b int) bool {
		return conditions[b].LastTransitionTime.Before(&conditions[a].LastTransitionTime)
	})

	mostRecentTimestamp := conditions[0].LastTransitionTime

	// loop through conditions until not in the most recent one anymore
	for index := range conditions {
		// only keep conditions with most recent timestamp
		if conditions[index].LastTransitionTime == mostRecentTimestamp {
			recentConditions = append(recentConditions, conditions[index])
		} else {
			break
		}
	}

	return recentConditions
}

func (r *ApplicationVolumeReplicationReconciler) processSubscriptionList(
	subscriptionList *subv1.SubscriptionList) (
	ramendrv1alpha1.SubscriptionPlacementDecisionMap, bool) {
	placementDecisions := ramendrv1alpha1.SubscriptionPlacementDecisionMap{}

	requeue := false

	for _, subscription := range subscriptionList.Items {
		// On the hub ignore any managed cluster subscriptions, as the hub maybe a managed cluster itself.
		// SubscriptionSubscribed means this subscription is child sitting in managed cluster
		// Placement.Local is true for a local subscription, and can be used in the absence of Status
		if subscription.Status.Phase == subv1.SubscriptionSubscribed ||
			(subscription.Spec.Placement != nil && subscription.Spec.Placement.Local != nil &&
				*subscription.Spec.Placement.Local) {
			r.Log.Info("skipping local subscription", "name", subscription.Name)

			continue
		}

		// Create a ManifestWork that represents the VolumeReplicationGroup CR for the hub to
		// deploy on the managed cluster. A manifest workload is defined as a set of Kubernetes
		// resources. The ManifestWork must be created in the cluster namespace on the hub, so that
		// agent on the corresponding managed cluster can access this resource and deploy on the
		// managed cluster. We create one ManifestWork for each VRG CR.
		homeCluster, peerCluster, err := r.createVRGManifestWork(subscription)
		if err != nil {
			r.Log.Info("failed to create or update ManifestWork for", "subscription", subscription.Name, "error", err)

			requeue = true

			continue
		}

		placementDecisions[subscription.Name] = &ramendrv1alpha1.SubscriptionPlacementDecision{
			HomeCluster: homeCluster,
			PeerCluster: peerCluster,
		}

		r.Log.Info("created VolumeReplicationGroup manifest for ", "subscription", subscription.Name,
			"HomeCluster", homeCluster, "PeerCluster", peerCluster)
	}

	return placementDecisions, requeue
}

func (r *ApplicationVolumeReplicationReconciler) updateAVRStatus(
	ctx context.Context,
	avr *ramendrv1alpha1.ApplicationVolumeReplication,
	placementDecisions ramendrv1alpha1.SubscriptionPlacementDecisionMap) error {
	r.Log.Info("updating AVR status")

	avr.Status = ramendrv1alpha1.ApplicationVolumeReplicationStatus{
		Decisions: placementDecisions,
	}
	if err := r.Client.Status().Update(ctx, avr); err != nil {
		return errorswrapper.Wrap(err, "failed to update AVR status")
	}

	return nil
}

func (r *ApplicationVolumeReplicationReconciler) createVRGManifestWork(
	subscription subv1.Subscription) (string, string, error) {
	const empty string = ""

	// Select cluster decisions from each Subscriptions of an app.
	homeCluster, peerCluster, err := r.selectPlacementDecision(&subscription)
	if err != nil {
		return empty, empty, fmt.Errorf("failed to get placement targets from subscription %s with error: %w",
			subscription.Name, err)
	}

	if homeCluster == "" {
		return empty, empty,
			fmt.Errorf("no home cluster configured in subscription %s", subscription.Name)
	}

	if peerCluster == "" {
		return empty, empty, fmt.Errorf("no peer cluster found for subscription %s", subscription.Name)
	}

	if err := r.createOrUpdateVRGRolesManifestWork(homeCluster); err != nil {
		r.Log.Error(err, "failed to create or update VolumeReplicationGroup Roles manifest")

		return empty, empty, err
	}

	if err := r.createOrUpdateVRGManifestWork(
		subscription.Name, subscription.Namespace, homeCluster); err != nil {
		r.Log.Error(err, "failed to create or update VolumeReplicationGroup manifest")

		return empty, empty, err
	}

	return homeCluster, peerCluster, nil
}

func (r *ApplicationVolumeReplicationReconciler) selectPlacementDecision(
	subscription *subv1.Subscription) (string, string, error) {
	// The subscription phase describes the phasing of the subscriptions. Propagated means
	// this subscription is the "parent" sitting in hub. Statuses is a map where the key is
	// the cluster name and value is the aggregated status
	if subscription.Status.Phase != subv1.SubscriptionPropagated || subscription.Status.Statuses == nil {
		return "", "", fmt.Errorf("subscription %s not ready", subscription.Name)
	}

	pl := subscription.Spec.Placement
	if pl == nil || pl.PlacementRef == nil {
		return "", "", fmt.Errorf("placement not set for subscription %s", subscription.Name)
	}

	plRef := pl.PlacementRef

	// if application subscription PlacementRef namespace is empty, then apply
	// the application subscription namespace as the PlacementRef namespace
	if plRef.Namespace == "" {
		plRef.Namespace = subscription.Namespace
	}

	// get the placement rule fo this subscription
	placementRule := &plrv1.PlacementRule{}

	err := r.Client.Get(context.TODO(),
		types.NamespacedName{Name: plRef.Name, Namespace: plRef.Namespace}, placementRule)
	if err != nil {
		return "", "", fmt.Errorf("failed to retrieve placementRule using placementRef %s/%s", plRef.Namespace, plRef.Name)
	}

	return r.extractHomeClusterAndPeerCluster(subscription, placementRule)
}

func (r *ApplicationVolumeReplicationReconciler) extractHomeClusterAndPeerCluster(
	subscription *subv1.Subscription, placementRule *plrv1.PlacementRule) (string, string, error) {
	subStatuses := subscription.Status.Statuses

	const clusterCount = 2

	// decisions := placementRule.Status.Decisions
	clmap, err := utils.PlaceByGenericPlacmentFields(
		r.Client, placementRule.Spec.GenericPlacementFields, nil, placementRule)
	if err != nil {
		return "", "", fmt.Errorf("failed to get cluster map for placement %s error: %w", placementRule.Name, err)
	}

	if len(clmap) != clusterCount {
		return "", "", fmt.Errorf("PlacementRule %s should have made 2 decisions. Found %d", placementRule.Name, len(clmap))
	}

	idx := 0

	clusters := make([]spokeClusterV1.ManagedCluster, clusterCount)
	for _, c := range clmap {
		clusters[idx] = *c
		idx++
	}

	d1 := clusters[0]
	d2 := clusters[1]

	var homeCluster string

	var peerCluster string

	switch {
	case subStatuses[d1.Name] != nil:
		homeCluster = d1.Name
		peerCluster = d2.Name
	case subStatuses[d2.Name] != nil:
		homeCluster = d2.Name
		peerCluster = d1.Name
	default:
		return "", "", fmt.Errorf("mismatch between placementRule %s decisions and subscription %s statuses",
			placementRule.Name, subscription.Name)
	}

	return homeCluster, peerCluster, nil
}

func (r *ApplicationVolumeReplicationReconciler) createOrUpdateVRGRolesManifestWork(namespace string) error {
	// TODO: Enhance to remember clusters where this has been checked to reduce repeated Gets of the object
	manifestWork, err := r.generateVRGRolesManifestWork(namespace)
	if err != nil {
		return err
	}

	return r.createOrUpdateManifestWork(manifestWork, namespace)
}

func (r *ApplicationVolumeReplicationReconciler) generateVRGRolesManifestWork(namespace string) (
	*ocmworkv1.ManifestWork,
	error) {
	vrgClusterRole, err := r.generateVRGClusterRoleManifest()
	if err != nil {
		r.Log.Error(err, "failed to generate VolumeReplicationGroup ClusterRole manifest", "namespace", namespace)

		return nil, err
	}

	vrgClusterRoleBinding, err := r.generateVRGClusterRoleBindingManifest()
	if err != nil {
		r.Log.Error(err, "failed to generate VolumeReplicationGroup ClusterRoleBinding manifest", "namespace", namespace)

		return nil, err
	}

	manifestwork := &ocmworkv1.ManifestWork{
		ObjectMeta: metav1.ObjectMeta{Name: "ramendr-vrg-roles", Namespace: namespace},
		Spec: ocmworkv1.ManifestWorkSpec{
			Workload: ocmworkv1.ManifestsTemplate{
				Manifests: []ocmworkv1.Manifest{
					*vrgClusterRole,
					*vrgClusterRoleBinding,
				},
			},
		},
	}

	return manifestwork, nil
}

func (r *ApplicationVolumeReplicationReconciler) generateVRGClusterRoleManifest() (*ocmworkv1.Manifest, error) {
	return r.generateManifest(&rbacv1.ClusterRole{
		TypeMeta:   metav1.TypeMeta{Kind: "ClusterRole", APIVersion: "rbac.authorization.k8s.io/v1"},
		ObjectMeta: metav1.ObjectMeta{Name: "open-cluster-management:klusterlet-work-sa:agent:volrepgroup-edit"},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{"ramendr.openshift.io"},
				Resources: []string{"volumereplicationgroups"},
				Verbs:     []string{"create", "get", "list", "update", "delete"},
			},
		},
	})
}

func (r *ApplicationVolumeReplicationReconciler) generateVRGClusterRoleBindingManifest() (*ocmworkv1.Manifest, error) {
	return r.generateManifest(&rbacv1.ClusterRoleBinding{
		TypeMeta:   metav1.TypeMeta{Kind: "ClusterRoleBinding", APIVersion: "rbac.authorization.k8s.io/v1"},
		ObjectMeta: metav1.ObjectMeta{Name: "open-cluster-management:klusterlet-work-sa:agent:volrepgroup-edit"},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      "klusterlet-work-sa",
				Namespace: "open-cluster-management-agent",
			},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     "open-cluster-management:klusterlet-work-sa:agent:volrepgroup-edit",
		},
	})
}

func (r *ApplicationVolumeReplicationReconciler) createOrUpdateVRGManifestWork(name string, namespace string,
	homeCluster string) error {
	manifestWork, err := r.generateVRGManifestWork(name, namespace, homeCluster)
	if err != nil {
		return err
	}

	return r.createOrUpdateManifestWork(manifestWork, homeCluster)
}

func (r *ApplicationVolumeReplicationReconciler) generateVRGManifestWork(name string, namespace string,
	homeCluster string) (*ocmworkv1.ManifestWork, error) {
	vrgClientManifest, err := r.generateVRGManifest(name, namespace)
	if err != nil {
		r.Log.Error(err, "failed to generate VolumeReplication")

		return nil, err
	}

	manifestwork := &ocmworkv1.ManifestWork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf(ManifestWorkNameFormat, name, namespace),
			Namespace: homeCluster, Labels: map[string]string{"app": "VRG"},
		},
		Spec: ocmworkv1.ManifestWorkSpec{
			Workload: ocmworkv1.ManifestsTemplate{
				Manifests: []ocmworkv1.Manifest{
					*vrgClientManifest,
				},
			},
		},
	}

	return manifestwork, nil
}

func (r *ApplicationVolumeReplicationReconciler) generateVRGManifest(name string,
	namespace string) (*ocmworkv1.Manifest, error) {
	return r.generateManifest(&ramendrv1alpha1.VolumeReplicationGroup{
		TypeMeta:   metav1.TypeMeta{Kind: "VolumeReplicationGroup", APIVersion: "ramendr.openshift.io/v1alpha1"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: ramendrv1alpha1.VolumeReplicationGroupSpec{
			PVCSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					"appclass":    "gold",
					"environment": "dev.AZ1",
				},
			},
			VolumeReplicationClass: "volume-rep-class",
			ReplicationState:       "Primary",
		},
	})
}

func (r *ApplicationVolumeReplicationReconciler) generateManifest(obj interface{}) (*ocmworkv1.Manifest, error) {
	objJSON, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal %v to JSON, error %w", obj, err)
	}

	manifest := &ocmworkv1.Manifest{}
	manifest.RawExtension = runtime.RawExtension{Raw: objJSON}

	return manifest, nil
}

func (r *ApplicationVolumeReplicationReconciler) createOrUpdateManifestWork(
	work *ocmworkv1.ManifestWork,
	managedClusternamespace string) error {
	found := &ocmworkv1.ManifestWork{}

	err := r.Client.Get(context.TODO(),
		types.NamespacedName{Name: work.Name, Namespace: managedClusternamespace},
		found)
	if err != nil {
		if !errors.IsNotFound(err) {
			r.Log.Error(err, "failed to fetch ManifestWork", "name", work.Name, "namespace", managedClusternamespace)

			return errorswrapper.Wrap(err, "failed to fetch ManifestWork")
		}

		r.Log.Info("Creating", "ManifestWork", work)

		return r.Client.Create(context.TODO(), work)
	}

	if !reflect.DeepEqual(found.Spec, work.Spec) {
		work.Spec.DeepCopyInto(&found.Spec)

		r.Log.Info("Updating", "ManifestWork", work)

		return r.Client.Update(context.TODO(), found)
	}

	return nil
}
