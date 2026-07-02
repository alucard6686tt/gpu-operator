/**
# Copyright (c) NVIDIA CORPORATION.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
**/

package controllers

import (
	"context"
	"fmt"
	"os"

	"github.com/NVIDIA/k8s-operator-libs/pkg/upgrade"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	gpuv1 "github.com/NVIDIA/gpu-operator/api/nvidia/v1"
	nvidiav1alpha1 "github.com/NVIDIA/gpu-operator/api/nvidia/v1alpha1"
	"github.com/NVIDIA/gpu-operator/internal/consts"
	nvidiadriverutil "github.com/NVIDIA/gpu-operator/internal/nvidiadriver"
)

const nodeLabelingControllerSingletonName = "cluster"

// NodeLabelingReconciler applies GPU-Operator related labels and annotations to Kubernetes nodes.
// All node label write operations for the GPU Operator are centralized here.
type NodeLabelingReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	Namespace string
	Log       logr.Logger
}

// nodeLabelingController holds per-reconcile state so that helper methods don't need to
// re-receive that state as arguments. clusterPolicy drives the device-plugin plane and
// gpuCluster the DRA plane; the two may coexist, with each node served by exactly
// one plane according to its nvidia.com/gpu-operator.mode label. defaultMode is the mode
// applied to GPU nodes that do not have the label yet.
type nodeLabelingController struct {
	client        client.Client
	namespace     string
	clusterPolicy *gpuv1.ClusterPolicy
	gpuCluster    *nvidiav1alpha1.GPUCluster
	defaultMode   string
	logger        logr.Logger
}

// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;update;patch

// Reconcile applies GPU-Operator related labels and annotations to all cluster nodes.
func (r *NodeLabelingReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	r.Log.Info("Reconciling node labels")

	// The ClusterPolicy (device-plugin plane) and GPUCluster (DRA plane) CRs may
	// coexist; neither existing means there is nothing to label.
	clusterPolicy, gpuCluster, err := resolveActiveConfig(ctx, r.Client)
	if err != nil {
		return reconcile.Result{}, err
	}
	if clusterPolicy == nil && gpuCluster == nil {
		r.Log.Info("No ClusterPolicy or GPUCluster CR exists, skipping node labeling")
		return reconcile.Result{}, nil
	}

	preferredMode := os.Getenv(consts.PreferredModeEnvName)
	if clusterPolicy != nil && gpuCluster != nil &&
		preferredMode != consts.GPUOperatorModeDevicePlugin && preferredMode != consts.GPUOperatorModeDRA {
		r.Log.Info("WARNING: both ClusterPolicy and GPUCluster exist but PREFERRED_MODE is unset or invalid; "+
			"defaulting new GPU nodes to the device-plugin stack", "PREFERRED_MODE", preferredMode)
	}

	nlc := &nodeLabelingController{
		client:        r.Client,
		namespace:     r.Namespace,
		clusterPolicy: clusterPolicy,
		gpuCluster:    gpuCluster,
		defaultMode:   resolveDefaultMode(clusterPolicy != nil, gpuCluster != nil, preferredMode),
		logger:        r.Log,
	}

	if err := nlc.labelGPUNodes(ctx); err != nil {
		return reconcile.Result{}, err
	}

	// Route each GPU node to its NVIDIADriver CR. Skipping this leaves the NVIDIADriver controller owning no nodes, and it
	// then removes the driver DaemonSet.
	usesNvidiaDriverCRD := nlc.gpuCluster != nil ||
		(nlc.clusterPolicy != nil && nlc.clusterPolicy.Spec.Driver.UseNvidiaDriverCRDType())
	if usesNvidiaDriverCRD {
		if _, err := nvidiadriverutil.AssignOwners(ctx, r.Client); err != nil {
			return reconcile.Result{}, fmt.Errorf("failed to assign NVIDIADriver owners to nodes: %w", err)
		}
		if err := nlc.labelNodesWithOrphanedDriverPods(ctx); err != nil {
			return reconcile.Result{}, fmt.Errorf("failed to label nodes with orphaned NVIDIA driver pods: %w", err)
		}
	}

	// The driver auto-upgrade annotation is derived from ClusterPolicy spec.
	if nlc.clusterPolicy != nil {
		if err := nlc.applyDriverAutoUpgradeAnnotation(ctx); err != nil {
			return reconcile.Result{}, err
		}
	}

	return reconcile.Result{}, nil
}

func (nlc *nodeLabelingController) labelGPUNodes(ctx context.Context) error {
	nodeList := &corev1.NodeList{}
	if err := nlc.client.List(ctx, nodeList); err != nil {
		return fmt.Errorf("unable to list nodes: %w", err)
	}

	for _, node := range nodeList.Items {
		original := node.DeepCopy()
		labels := node.GetLabels()
		modified := false

		if nlc.reconcileCommonGPULabel(labels, node.Name) {
			node.SetLabels(labels)
			modified = true
		}

		if nlc.reconcileModeLabel(labels, node.Name) {
			node.SetLabels(labels)
			modified = true
		}

		if nlc.updateGPUStateLabels(labels, node.Name) {
			node.SetLabels(labels)
			modified = true
		}

		if modified {
			if err := nlc.client.Patch(ctx, &node, client.MergeFrom(original)); err != nil {
				return fmt.Errorf("unable to label node %s: %w", node.Name, err)
			}
		}
	}
	return nil
}

// reconcileCommonGPULabel keeps nvidia.com/gpu.present in sync with NFD GPU PCI labels.
// Returns true if labels were modified.
func (nlc *nodeLabelingController) reconcileCommonGPULabel(labels map[string]string, nodeName string) bool {
	if !hasCommonGPULabel(labels) && hasGPULabels(labels) {
		nlc.logger.Info("Node has GPU(s), setting common GPU label", "NodeName", nodeName)
		labels[commonGPULabelKey] = commonGPULabelValue
		return true
	}
	if hasCommonGPULabel(labels) && !hasGPULabels(labels) {
		nlc.logger.Info("Node no longer has GPUs, clearing GPU labels", "NodeName", nodeName)
		labels[commonGPULabelKey] = "false"
		return true
	}
	return false
}

// reconcileModeLabel writes nvidia.com/gpu-operator.mode on GPU nodes that do not have it
// yet. An existing value is never overwritten (or removed), whether set by a previous
// reconcile or manually by a user: changing the cluster configuration or PREFERRED_MODE
// must not migrate nodes that are already serving GPUs through one stack. Returns true if
// labels were modified.
func (nlc *nodeLabelingController) reconcileModeLabel(labels map[string]string, nodeName string) bool {
	if !hasCommonGPULabel(labels) {
		return false
	}
	if _, ok := labels[consts.GPUOperatorModeLabelKey]; ok {
		return false
	}
	if nlc.defaultMode == "" {
		return false
	}
	nlc.logger.Info("Setting GPU Operator mode label", "NodeName", nodeName,
		"Label", consts.GPUOperatorModeLabelKey, "Value", nlc.defaultMode)
	labels[consts.GPUOperatorModeLabelKey] = nlc.defaultMode
	return true
}

// updateGPUStateLabels syncs nvidia.com/gpu.deploy.* labels and sets the MIG config label when
// appropriate. Which label set is applied follows the node's nvidia.com/gpu-operator.mode
// label; deploy labels exclusive to the other plane are swept away, while shared and
// unrecognized deploy labels are left alone. If the node does not have the common GPU
// label, all state labels are removed. Returns true if labels were modified.
func (nlc *nodeLabelingController) updateGPUStateLabels(labels map[string]string, nodeName string) bool {
	if !hasCommonGPULabel(labels) {
		return removeAllGPUStateLabels(labels)
	}

	switch labels[consts.GPUOperatorModeLabelKey] {
	case consts.GPUOperatorModeDRA:
		if nlc.gpuCluster == nil {
			return false
		}
		modified := nlc.sweepOtherPlaneStateLabels(labels, devicePluginOnlyStateLabelKeys(), nodeName)
		if updateGPUClusterStateLabels(labels) {
			modified = true
		}
		return modified
	case consts.GPUOperatorModeDevicePlugin:
		if nlc.clusterPolicy == nil {
			return false
		}
	default:
		// Unlabeled (or unrecognized mode): apply no deploy labels. The mode nodeSelector
		// gate on every operand keeps the node empty until a mode is set.
		return false
	}

	cp := nlc.clusterPolicy
	sandboxEnabled := cp != nil && cp.Spec.SandboxWorkloads.IsEnabled()
	sandboxMode := ""
	if cp != nil {
		sandboxMode = cp.Spec.SandboxWorkloads.Mode
	}

	config, err := getWorkloadConfig(labels, sandboxEnabled)
	if err != nil {
		nlc.logger.Info("WARNING: failed to get GPU workload config for node; using default",
			"NodeName", nodeName, "SandboxEnabled", sandboxEnabled,
			"Error", err, "defaultGPUWorkloadConfig", defaultGPUWorkloadConfig)
	}
	gpuWorkloadConfig := &gpuWorkloadConfiguration{
		config:      config,
		sandboxMode: sandboxMode,
		node:        nodeName,
		log:         nlc.logger,
	}
	modified := gpuWorkloadConfig.updateGPUStateLabels(labels)

	if cp != nil && cp.Spec.MIGManager.IsEnabled() && hasMIGCapableGPU(labels) && !hasMIGConfigLabel(labels) {
		migConfigDefault := ""
		if cp.Spec.MIGManager.Config != nil {
			migConfigDefault = cp.Spec.MIGManager.Config.Default
		}
		if migConfigDefault == migConfigDisabledValue {
			nlc.logger.Info("Setting MIG config label", "NodeName", nodeName,
				"Label", migConfigLabelKey, "Value", migConfigDisabledValue)
			labels[migConfigLabelKey] = migConfigDisabledValue
			modified = true
		}
	}
	return modified
}

// updateGPUClusterStateLabels is the GPUCluster analogue of the ClusterPolicy
// gpuWorkloadConfiguration state-label logic: it sets the DRA operand deploy labels on a GPU
// node (removal once the GPUs are gone is handled by removeAllGPUStateLabels). Like the
// ClusterPolicy path it honors an existing value (set only when absent) so the
// k8s-driver-manager can pause an operand by flipping its label to drain it off a node
// during a driver reload. Returns true if modified.
func updateGPUClusterStateLabels(labels map[string]string) bool {
	modified := false
	for key, value := range gpuClusterStateLabels {
		if _, ok := labels[key]; !ok {
			labels[key] = value
			modified = true
		}
	}
	return modified
}

// sweepOtherPlaneStateLabels deletes the given other-plane deploy-label keys, value-blind.
// Keys outside deleteKeys are never touched, so k8s-driver-manager pause state on the
// owning plane's keys survives. Returns true if labels were modified.
func (nlc *nodeLabelingController) sweepOtherPlaneStateLabels(labels map[string]string, deleteKeys map[string]bool, nodeName string) bool {
	modified := false
	for key := range deleteKeys {
		if _, ok := labels[key]; !ok {
			continue
		}
		nlc.logger.Info("Deleting node label", "NodeName", nodeName, "Label", key)
		delete(labels, key)
		modified = true
	}
	return modified
}

func (nlc *nodeLabelingController) setDriverAutoUpgradeAnnotation(ctx context.Context, node *corev1.Node, autoUpgradeEnabled bool) error {
	annotationValue, annotationExists := node.Annotations[driverAutoUpgradeAnnotationKey]
	updateRequired := false
	if autoUpgradeEnabled {
		updateRequired = !annotationExists || annotationValue != "true"
	} else {
		updateRequired = annotationExists
	}
	if !updateRequired {
		return nil
	}

	original := node.DeepCopy()
	if node.Annotations == nil {
		node.Annotations = map[string]string{}
	}
	if autoUpgradeEnabled {
		node.Annotations[driverAutoUpgradeAnnotationKey] = "true"
	} else {
		delete(node.Annotations, driverAutoUpgradeAnnotationKey)
	}
	if err := nlc.client.Patch(ctx, node, client.MergeFrom(original)); err != nil {
		nlc.logger.Error(err, "Failed to patch driver auto-upgrade annotation",
			"node", node.Name, "enabled", autoUpgradeEnabled)
		return err
	}

	return nil
}

// applyDriverAutoUpgradeAnnotation sets or clears the driver auto-upgrade annotation on GPU nodes.
func (nlc *nodeLabelingController) applyDriverAutoUpgradeAnnotation(ctx context.Context) error {
	cp := nlc.clusterPolicy

	if cp.Spec.Driver.UseNvidiaDriverCRDType() && !cp.Spec.SandboxWorkloads.IsEnabled() {
		return nlc.applyDriverAutoUpgradeAnnotationForNVD(ctx)
	}

	autoUpgradeEnabled := cp.Spec.Driver.IsEnabled() &&
		cp.Spec.Driver.IsAutoUpgradeEnabled() &&
		!cp.Spec.SandboxWorkloads.IsEnabled()

	nodeList := &corev1.NodeList{}
	if err := nlc.client.List(ctx, nodeList, client.MatchingLabels{consts.GPUPresentLabel: "true"}); err != nil {
		return fmt.Errorf("unable to list nodes: %w", err)
	}

	for _, node := range nodeList.Items {
		err := nlc.setDriverAutoUpgradeAnnotation(ctx, &node, autoUpgradeEnabled)
		if err != nil {
			return fmt.Errorf("failed to set driver auto-upgrade annotation on node %q: %w", node.Name, err)
		}
	}
	return nil
}

func (nlc *nodeLabelingController) applyDriverAutoUpgradeAnnotationForNVD(ctx context.Context) error {
	nvidiaDriverList := &nvidiav1alpha1.NVIDIADriverList{}
	if err := nlc.client.List(ctx, nvidiaDriverList); err != nil {
		return fmt.Errorf("failed to list NVIDIADriver instances: %w", err)
	}

	for _, nvd := range nvidiaDriverList.Items {
		nodeList := &corev1.NodeList{}
		if err := nlc.client.List(ctx, nodeList, client.MatchingLabels{consts.NVIDIADriverOwnerLabel: nvd.Name}); err != nil {
			nlc.logger.Error(err, "Failed to list nodes for NVIDIADriver", "name", nvd.Name)
			return err
		}
		autoUpgradeEnabled := nvd.Spec.GetUpgradePolicyWithDefaults().AutoUpgrade
		for _, node := range nodeList.Items {
			err := nlc.setDriverAutoUpgradeAnnotation(ctx, &node, autoUpgradeEnabled)
			if err != nil {
				return fmt.Errorf("failed to set driver auto-upgrade annotation on node %q: %w", node.Name, err)
			}
		}
	}

	return nil
}

// labelNodesWithOrphanedDriverPods marks nodes that still have unowned (orphaned) ClusterPolicy
// driver pods so the upgrade controller can replace them in the normal upgrade flow.
func (nlc *nodeLabelingController) labelNodesWithOrphanedDriverPods(ctx context.Context) error {
	nvidiaDrivers := &nvidiav1alpha1.NVIDIADriverList{}
	if err := nlc.client.List(ctx, nvidiaDrivers); err != nil {
		return fmt.Errorf("failed to list NVIDIADriver CRs: %w", err)
	}
	if len(nvidiaDrivers.Items) == 0 {
		return nil
	}

	pods := &corev1.PodList{}
	if err := nlc.client.List(ctx, pods,
		client.InNamespace(nlc.namespace),
		client.MatchingLabels{AppComponentLabelKey: DriverAppComponentLabelValue},
	); err != nil {
		return fmt.Errorf("failed to list NVIDIA driver pods: %w", err)
	}

	for _, pod := range pods.Items {
		if len(pod.OwnerReferences) > 0 || pod.Status.Phase != corev1.PodRunning || pod.Spec.NodeName == "" {
			continue
		}

		node := &corev1.Node{}
		if err := nlc.client.Get(ctx, types.NamespacedName{Name: pod.Spec.NodeName}, node); err != nil {
			nlc.logger.Error(err, "failed to get node for orphaned driver pod", "pod", pod.Name, "node", pod.Spec.NodeName)
			continue
		}
		if !nodeOwnedByNVIDIADriver(node, nvidiaDrivers.Items) {
			continue
		}

		upgradeStateLabel := upgrade.GetUpgradeStateLabelKey()
		upgradeState := upgrade.UpgradeStateUnknown
		if node.Labels != nil {
			upgradeState = node.Labels[upgradeStateLabel]
		}
		if !isDriverUpgradeRequestAllowed(upgradeState) {
			continue
		}

		original := node.DeepCopy()
		if node.Labels == nil {
			node.Labels = map[string]string{}
		}
		node.Labels[upgradeStateLabel] = upgrade.UpgradeStateUpgradeRequired
		if err := nlc.client.Patch(ctx, node, client.MergeFrom(original)); err != nil {
			return fmt.Errorf("failed to label node %q for orphaned driver pod %q: %w", node.Name, pod.Name, err)
		}
	}
	return nil
}

// nodeOwnedByNVIDIADriver returns true when the node has an owner label matching a live NVIDIADriver.
func nodeOwnedByNVIDIADriver(node *corev1.Node, nvidiaDrivers []nvidiav1alpha1.NVIDIADriver) bool {
	if node.Labels == nil || node.Labels[consts.NVIDIADriverOwnerLabel] == "" {
		return false
	}
	for _, nvidiaDriver := range nvidiaDrivers {
		if nvidiaDriver.HasDeletionTimestamp() {
			continue
		}
		if node.Labels[consts.NVIDIADriverOwnerLabel] == nvidiaDriver.Name {
			return true
		}
	}
	return false
}

// isDriverUpgradeRequestAllowed returns true when migration can request a driver upgrade
// without overwriting an active or failed upgrade state.
func isDriverUpgradeRequestAllowed(upgradeState string) bool {
	return upgradeState == upgrade.UpgradeStateUnknown || upgradeState == upgrade.UpgradeStateDone
}

// SetupWithManager registers the NodeLabelingReconciler with the controller-runtime manager.
func (r *NodeLabelingReconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager) error {
	mapToSingleton := func(_ context.Context, _ client.Object) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: nodeLabelingControllerSingletonName}}}
	}

	c, err := controller.New("node-labeling-controller", mgr, controller.Options{
		Reconciler:              r,
		MaxConcurrentReconciles: 1,
		RateLimiter:             workqueue.NewTypedItemExponentialFailureRateLimiter[reconcile.Request](minDelayCR, maxDelayCR),
	})
	if err != nil {
		return fmt.Errorf("error creating node-labeling controller: %w", err)
	}

	clusterPolicyMapFn := func(ctx context.Context, cp *gpuv1.ClusterPolicy) []reconcile.Request {
		return mapToSingleton(ctx, cp)
	}
	if err := c.Watch(source.Kind(
		mgr.GetCache(),
		&gpuv1.ClusterPolicy{},
		handler.TypedEnqueueRequestsFromMapFunc(clusterPolicyMapFn),
		predicate.TypedGenerationChangedPredicate[*gpuv1.ClusterPolicy]{},
	)); err != nil {
		return fmt.Errorf("error watching ClusterPolicy: %w", err)
	}

	// Watch GPUCluster so GPU nodes are (re)labeled for the DRA stack as the CR is
	// created or removed, mirroring the ClusterPolicy watch above.
	gpuClusterMapFn := func(ctx context.Context, gc *nvidiav1alpha1.GPUCluster) []reconcile.Request {
		return mapToSingleton(ctx, gc)
	}
	if err := c.Watch(source.Kind(
		mgr.GetCache(),
		&nvidiav1alpha1.GPUCluster{},
		handler.TypedEnqueueRequestsFromMapFunc(gpuClusterMapFn),
		predicate.TypedGenerationChangedPredicate[*nvidiav1alpha1.GPUCluster]{},
	)); err != nil {
		return fmt.Errorf("error watching GPUCluster: %w", err)
	}

	// Watch NVIDIADriver including delete events so owner labels are cleaned up promptly.
	nvidiaDriverMapFn := func(ctx context.Context, nd *nvidiav1alpha1.NVIDIADriver) []reconcile.Request {
		return mapToSingleton(ctx, nd)
	}
	if err := c.Watch(source.Kind(
		mgr.GetCache(),
		&nvidiav1alpha1.NVIDIADriver{},
		handler.TypedEnqueueRequestsFromMapFunc(nvidiaDriverMapFn),
		predicate.TypedGenerationChangedPredicate[*nvidiav1alpha1.NVIDIADriver]{},
	)); err != nil {
		return fmt.Errorf("error watching NVIDIADriver: %w", err)
	}

	nodePredicate := predicate.TypedFuncs[*corev1.Node]{
		CreateFunc: func(e event.TypedCreateEvent[*corev1.Node]) bool {
			labels := e.Object.GetLabels()
			return hasGPULabels(labels)
		},
		UpdateFunc: func(e event.TypedUpdateEvent[*corev1.Node]) bool {
			newLabels := e.ObjectNew.GetLabels()
			oldLabels := e.ObjectOld.GetLabels()
			nodeName := e.ObjectNew.GetName()

			gpuCommonLabelMissing := hasGPULabels(newLabels) && !hasCommonGPULabel(newLabels)
			gpuCommonLabelOutdated := !hasGPULabels(newLabels) && hasCommonGPULabel(newLabels)
			commonOperandsLabelChanged := hasOperandsDisabled(oldLabels) != hasOperandsDisabled(newLabels)

			modeLabelMissing := hasCommonGPULabel(newLabels) && newLabels[consts.GPUOperatorModeLabelKey] == ""
			modeLabelChanged := oldLabels[consts.GPUOperatorModeLabelKey] != newLabels[consts.GPUOperatorModeLabelKey]

			oldGPUWorkloadConfig, _ := getWorkloadConfig(oldLabels, true)
			newGPUWorkloadConfig, _ := getWorkloadConfig(newLabels, true)
			gpuWorkloadConfigLabelChanged := oldGPUWorkloadConfig != newGPUWorkloadConfig

			oldOSTreeLabel := oldLabels[nfdOSTreeVersionLabelKey]
			newOSTreeLabel := newLabels[nfdOSTreeVersionLabelKey]
			osTreeLabelChanged := oldOSTreeLabel != newOSTreeLabel

			nvidiaDriverOwnerLabelChanged := oldLabels[consts.NVIDIADriverOwnerLabel] != newLabels[consts.NVIDIADriverOwnerLabel]

			needsUpdate := gpuCommonLabelMissing ||
				gpuCommonLabelOutdated ||
				commonOperandsLabelChanged ||
				modeLabelMissing ||
				modeLabelChanged ||
				gpuWorkloadConfigLabelChanged ||
				osTreeLabelChanged ||
				nvidiaDriverOwnerLabelChanged

			// When an NVIDIADriver daemonset pod is running on the node, check if any
			// label which is configured in the NVIDIADriver's node selector has changed.
			nvidiaDriverNodeSelectorLabelChanged := false
			if !needsUpdate && newLabels[consts.NVIDIADriverOwnerLabel] != "" {
				name := newLabels[consts.NVIDIADriverOwnerLabel]
				nvidiaDriver := &nvidiav1alpha1.NVIDIADriver{}
				err := r.Get(ctx, types.NamespacedName{Name: name}, nvidiaDriver)
				if err != nil {
					r.Log.Error(err, "failed to get NVIDIADriver object that owns this node", "name", name, "node", nodeName)
					return false
				}
				for key := range nvidiaDriver.Spec.NodeSelector {
					if oldLabels[key] != newLabels[key] {
						nvidiaDriverNodeSelectorLabelChanged = true
						needsUpdate = true
						break
					}
				}
			}

			if needsUpdate {
				r.Log.Info("Node needs an update",
					"name", nodeName,
					"gpuCommonLabelMissing", gpuCommonLabelMissing,
					"gpuCommonLabelOutdated", gpuCommonLabelOutdated,
					"commonOperandsLabelChanged", commonOperandsLabelChanged,
					"modeLabelMissing", modeLabelMissing,
					"modeLabelChanged", modeLabelChanged,
					"gpuWorkloadConfigLabelChanged", gpuWorkloadConfigLabelChanged,
					"osTreeLabelChanged", osTreeLabelChanged,
					"nvidiaDriverOwnerLabelChanged", nvidiaDriverOwnerLabelChanged,
					"nvidiaDriverNodeSelectorLabelChanged", nvidiaDriverNodeSelectorLabelChanged,
				)
			}
			return needsUpdate
		},
		DeleteFunc: func(e event.TypedDeleteEvent[*corev1.Node]) bool {
			return false
		},
	}
	nodeMapFn := func(ctx context.Context, n *corev1.Node) []reconcile.Request {
		return mapToSingleton(ctx, n)
	}
	if err := c.Watch(source.Kind(
		mgr.GetCache(),
		&corev1.Node{},
		handler.TypedEnqueueRequestsFromMapFunc(nodeMapFn),
		nodePredicate,
	)); err != nil {
		return fmt.Errorf("error watching Nodes: %w", err)
	}

	// Trigger on driver pods becoming Running so orphaned pods are detected promptly.
	podPredicate := predicate.TypedFuncs[*corev1.Pod]{
		CreateFunc: func(e event.TypedCreateEvent[*corev1.Pod]) bool {
			return e.Object.GetLabels()[AppComponentLabelKey] == DriverAppComponentLabelValue
		},
		UpdateFunc: func(e event.TypedUpdateEvent[*corev1.Pod]) bool {
			if e.ObjectNew.GetLabels()[AppComponentLabelKey] != DriverAppComponentLabelValue {
				return false
			}
			return e.ObjectOld.Status.Phase != corev1.PodRunning &&
				e.ObjectNew.Status.Phase == corev1.PodRunning
		},
		DeleteFunc: func(e event.TypedDeleteEvent[*corev1.Pod]) bool {
			return false
		},
	}
	podMapFn := func(ctx context.Context, p *corev1.Pod) []reconcile.Request {
		return mapToSingleton(ctx, p)
	}
	if err := c.Watch(source.Kind(
		mgr.GetCache(),
		&corev1.Pod{},
		handler.TypedEnqueueRequestsFromMapFunc(podMapFn),
		podPredicate,
	)); err != nil {
		return fmt.Errorf("error watching Pods: %w", err)
	}

	return nil
}
