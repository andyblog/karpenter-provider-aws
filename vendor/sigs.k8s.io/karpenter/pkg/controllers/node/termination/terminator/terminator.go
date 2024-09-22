/*
Copyright The Kubernetes Authors.

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

package terminator

import (
	"context"
	"fmt"

	"github.com/samber/lo"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	nodeutil "sigs.k8s.io/karpenter/pkg/utils/node"

	"sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	podutil "sigs.k8s.io/karpenter/pkg/utils/pod"
)

type Terminator struct {
	clock                  clock.Clock
	kubeClient             client.Client
	nodeRestartDeployments map[string]map[string]struct{}
	evictionQueue          *Queue
}

func NewTerminator(clk clock.Clock, kubeClient client.Client, eq *Queue) *Terminator {
	return &Terminator{
		clock:                  clk,
		kubeClient:             kubeClient,
		nodeRestartDeployments: make(map[string]map[string]struct{}),
		evictionQueue:          eq,
	}
}

// Taint idempotently adds the karpenter.sh/disruption taint to a node with a NodeClaim
func (t *Terminator) Taint(ctx context.Context, node *v1.Node) error {
	stored := node.DeepCopy()
	// If the taint already has the karpenter.sh/disruption=disrupting:NoSchedule taint, do nothing.
	if _, ok := lo.Find(node.Spec.Taints, func(t v1.Taint) bool {
		return v1beta1.IsDisruptingTaint(t)
	}); !ok {
		// If the taint key exists (but with a different value or effect), remove it.
		node.Spec.Taints = lo.Reject(node.Spec.Taints, func(t v1.Taint, _ int) bool {
			return t.Key == v1beta1.DisruptionTaintKey
		})
		node.Spec.Taints = append(node.Spec.Taints, v1beta1.DisruptionNoScheduleTaint)
	}
	// Adding this label to the node ensures that the node is removed from the load-balancer target group
	// while it is draining and before it is terminated. This prevents 500s coming prior to health check
	// when the load balancer controller hasn't yet determined that the node and underlying connections are gone
	// https://github.com/aws/aws-node-termination-handler/issues/316
	// https://github.com/aws/karpenter/pull/2518
	node.Labels = lo.Assign(node.Labels, map[string]string{
		v1.LabelNodeExcludeBalancers: "karpenter",
	})
	if !equality.Semantic.DeepEqual(node, stored) {
		if err := t.kubeClient.Patch(ctx, node, client.StrategicMergeFrom(stored)); err != nil {
			return err
		}
		log.FromContext(ctx).Info("tainted node")
	}
	return nil
}

// Drain evicts pods from the node and returns true when all pods are evicted
// https://kubernetes.io/docs/concepts/architecture/nodes/#graceful-node-shutdown
func (t *Terminator) Drain(ctx context.Context, node *v1.Node) error {
	pods, err := nodeutil.GetPods(ctx, t.kubeClient, node)
	if err != nil {
		return fmt.Errorf("listing pods on node, %w", err)
	}

	// If the deployment corresponding to the pod has only one pod,
	// or all the pods of the deployment are on this node,
	// restarting the deployment can reduce the service interruption time.
	restartDeployments, drainPods, err := t.GetRestartdeploymentsAndDrainPods(ctx, pods, node.Name)
	if err != nil {
		return fmt.Errorf("get deployment and drain pod from node %w", err)
	}
	if err = t.RestartDeployments(ctx, restartDeployments, node.Name); err != nil {
		return fmt.Errorf("restart deployments from node %s, %w", node.Name, err)
	}

	for _, pod := range drainPods {
		log.FromContext(ctx).WithValues("name", pod.Name).Info("####drainPods")
	}

	// evictablePods are pods that aren't yet terminating are eligible to have the eviction API called against them
	evictablePods := lo.Filter(drainPods, func(p *v1.Pod, _ int) bool { return podutil.IsEvictable(p) })
	t.Evict(evictablePods)

	// podsWaitingEvictionCount are  the number of pods that either haven't had eviction called against them yet
	// or are still actively terminated and haven't exceeded their termination grace period yet
	podsWaitingEvictionCount := lo.CountBy(pods, func(p *v1.Pod) bool { return podutil.IsWaitingEviction(p, t.clock) })
	if podsWaitingEvictionCount > 0 {
		log.FromContext(ctx).WithValues("nums", podsWaitingEvictionCount).Info("pods are waiting to be evicted")
		return NewNodeDrainError(fmt.Errorf("%d pods are waiting to be evicted", len(pods)))
	}

	delete(t.nodeRestartDeployments, node.Name)
	return nil
}

func (t *Terminator) Evict(pods []*v1.Pod) {
	// 1. Prioritize noncritical pods, non-daemon pods https://kubernetes.io/docs/concepts/architecture/nodes/#graceful-node-shutdown
	var criticalNonDaemon, criticalDaemon, nonCriticalNonDaemon, nonCriticalDaemon []*v1.Pod
	for _, pod := range pods {
		if pod.Spec.PriorityClassName == "system-cluster-critical" || pod.Spec.PriorityClassName == "system-node-critical" {
			if podutil.IsOwnedByDaemonSet(pod) {
				criticalDaemon = append(criticalDaemon, pod)
			} else {
				criticalNonDaemon = append(criticalNonDaemon, pod)
			}
		} else {
			if podutil.IsOwnedByDaemonSet(pod) {
				nonCriticalDaemon = append(nonCriticalDaemon, pod)
			} else {
				nonCriticalNonDaemon = append(nonCriticalNonDaemon, pod)
			}
		}
	}
	// 2. Evict in order:
	// a. non-critical non-daemonsets
	// b. non-critical daemonsets
	// c. critical non-daemonsets
	// d. critical daemonsets
	if len(nonCriticalNonDaemon) != 0 {
		t.evictionQueue.Add(nonCriticalNonDaemon...)
	} else if len(nonCriticalDaemon) != 0 {
		t.evictionQueue.Add(nonCriticalDaemon...)
	} else if len(criticalNonDaemon) != 0 {
		t.evictionQueue.Add(criticalNonDaemon...)
	} else if len(criticalDaemon) != 0 {
		t.evictionQueue.Add(criticalDaemon...)
	}
}

func (t *Terminator) GetDeploymentFromPod(ctx context.Context, pod *v1.Pod) (*appsv1.Deployment, error) {
	rs, err := t.getOwnerReplicaSet(ctx, pod)
	if err != nil {
		return nil, fmt.Errorf("failed to get ReplicaSet from Pod: %w", err)
	}
	if rs == nil {
		return nil, nil
	}

	deployment, err := t.getOwnerDeployment(ctx, rs)
	if err != nil {
		return nil, fmt.Errorf("failed to get Deployment from ReplicaSet: %w", err)
	}
	return deployment, nil

}

func (t *Terminator) getOwnerReplicaSet(ctx context.Context, pod *v1.Pod) (*appsv1.ReplicaSet, error) {
	for _, ownerRef := range pod.GetOwnerReferences() {
		if ownerRef.Controller != nil && ownerRef.Kind == "ReplicaSet" {
			rs := &appsv1.ReplicaSet{}
			if err := t.kubeClient.Get(ctx, client.ObjectKey{Name: ownerRef.Name, Namespace: pod.Namespace}, rs); err != nil {
				return nil, fmt.Errorf("get ReplicaSet: %w", err)
			}
			return rs, nil
		}
	}

	return nil, nil
}

func (t *Terminator) getOwnerDeployment(ctx context.Context, rs *appsv1.ReplicaSet) (*appsv1.Deployment, error) {
	for _, ownerRef := range rs.GetOwnerReferences() {
		if ownerRef.Controller != nil && ownerRef.Kind == "Deployment" {
			deployment := &appsv1.Deployment{}
			if err := t.kubeClient.Get(ctx, client.ObjectKey{Name: ownerRef.Name, Namespace: rs.Namespace}, deployment); err != nil {
				return nil, fmt.Errorf("get Deployment: %w", err)
			}
			return deployment, nil
		}
	}

	return nil, nil
}

func (t *Terminator) RestartDeployments(ctx context.Context, deployments []*appsv1.Deployment, nodeName string) error {
	var updateErrors []error

	for _, deployment := range deployments {
		if deployment.Spec.Template.Annotations == nil {
			deployment.Spec.Template.Annotations = make(map[string]string)
		}
		restartedNode, exists := deployment.Spec.Template.Annotations["kubectl.kubernetes.io/restartedNode"]
		if exists && restartedNode == nodeName {
			continue
		}

		log.FromContext(ctx).WithValues("deployment", deployment.Name).Info("restart deployment")
		t.nodeRestartDeployments[nodeName][deployment.Namespace+"/"+deployment.Name] = struct{}{}

		deployment.Spec.Template.Annotations["kubectl.kubernetes.io/restartedNode"] = nodeName
		if err := t.kubeClient.Update(ctx, deployment); err != nil {
			updateErrors = append(updateErrors, err)
			continue
		}

	}

	if len(updateErrors) > 0 {
		return fmt.Errorf("failed to restart some deployment: %v", updateErrors)
	}

	return nil
}

func (t *Terminator) GetRestartdeploymentsAndDrainPods(ctx context.Context, pods []*v1.Pod, nodeName string) ([]*appsv1.Deployment, []*v1.Pod, error) {
	var drainPods []*v1.Pod
	var restartDeployments []*appsv1.Deployment
	nodeDeploymentReplicas := make(map[string]int32)
	deploymentCache := make(map[string]*appsv1.Deployment)
	uniqueDeployments := make(map[string]struct{})

	for _, pod := range pods {
		deployment, err := t.getDeploymentFromCache(ctx, pod, deploymentCache)
		if err != nil {
			return nil, nil, err
		}
		if deployment != nil {
			nodeDeploymentReplicas[deployment.Namespace+"/"+deployment.Name]++
		}
	}

	for _, pod := range pods {
		deployment := deploymentCache[pod.Namespace+"/"+pod.Name]

		key := deployment.Namespace + "/" + deployment.Name
		if deployment != nil && nodeDeploymentReplicas[key] == *deployment.Spec.Replicas {
			// If a deployment has multiple pods on this node, there will be multiple deployments here, and deduplication is required.
			if _, exists := uniqueDeployments[key]; !exists {
				uniqueDeployments[key] = struct{}{}
				restartDeployments = append(restartDeployments, deployment)
			}
			continue
		}

		if deployment != nil {
			if _, exists := t.nodeRestartDeployments[nodeName][key]; exists {
				continue

			}
		}

		drainPods = append(drainPods, pod)
	}

	return restartDeployments, drainPods, nil
}

func (t *Terminator) getDeploymentFromCache(ctx context.Context, pod *v1.Pod, cache map[string]*appsv1.Deployment) (*appsv1.Deployment, error) {
	key := pod.Namespace + "/" + pod.Name
	if deployment, exists := cache[key]; exists {
		return deployment, nil
	}

	deployment, err := t.GetDeploymentFromPod(ctx, pod)
	if err != nil {
		return nil, err
	}

	cache[key] = deployment
	return deployment, nil
}
