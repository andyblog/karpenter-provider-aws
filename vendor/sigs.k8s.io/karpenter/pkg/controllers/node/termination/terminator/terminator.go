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
	"encoding/json"
	"time"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"sigs.k8s.io/karpenter/pkg/events"
	nodeutil "sigs.k8s.io/karpenter/pkg/utils/node"
	appsv1 "k8s.io/api/apps/v1"

	terminatorevents "sigs.k8s.io/karpenter/pkg/controllers/node/termination/terminator/events"
	podutil "sigs.k8s.io/karpenter/pkg/utils/pod"
)

type Terminator struct {
	clock         clock.Clock
	kubeClient    client.Client
	evictionQueue *Queue
	recorder      events.Recorder
}

func NewTerminator(clk clock.Clock, kubeClient client.Client, eq *Queue, recorder events.Recorder) *Terminator {
	return &Terminator{
		clock:         clk,
		kubeClient:    kubeClient,
		evictionQueue: eq,
		recorder:      recorder,
	}
}

// Taint idempotently adds a given taint to a node with a NodeClaim
func (t *Terminator) Taint(ctx context.Context, node *corev1.Node, taint corev1.Taint) error {
	stored := node.DeepCopy()
	// If the node already has the correct taint (key and effect), do nothing.
	if _, ok := lo.Find(node.Spec.Taints, func(t corev1.Taint) bool {
		return t.MatchTaint(&taint)
	}); !ok {
		// Otherwise, if the taint key exists (but with a different effect), remove it.
		node.Spec.Taints = lo.Reject(node.Spec.Taints, func(t corev1.Taint, _ int) bool {
			return t.Key == taint.Key
		})
		node.Spec.Taints = append(node.Spec.Taints, taint)
	}
	// Adding this label to the node ensures that the node is removed from the load-balancer target group
	// while it is draining and before it is terminated. This prevents 500s coming prior to health check
	// when the load balancer controller hasn't yet determined that the node and underlying connections are gone
	// https://github.com/aws/aws-node-termination-handler/issues/316
	// https://github.com/aws/karpenter/pull/2518
	node.Labels = lo.Assign(node.Labels, map[string]string{
		corev1.LabelNodeExcludeBalancers: "karpenter",
	})
	if !equality.Semantic.DeepEqual(node, stored) {
		if err := t.kubeClient.Patch(ctx, node, client.StrategicMergeFrom(stored)); err != nil {
			return err
		}
		taintValues := []any{
			"taint.Key", taint.Key,
			"taint.Value", taint.Value,
		}
		if len(string(taint.Effect)) > 0 {
			taintValues = append(taintValues, "taint.Effect", taint.Effect)
		}
		log.FromContext(ctx).WithValues(taintValues...).Info("tainted node")
	}
	return nil
}



func (t *Terminator) GetDeploymentFromPod(ctx context.Context, pod *corev1.Pod) (*appsv1.Deployment,error) {
	var rsName string
	for _, ownerRef := range pod.GetOwnerReferences() {
		if *ownerRef.Controller && ownerRef.Kind == "ReplicaSet" {
			rsName = ownerRef.Name
			break
		}
	}
	if rsName == "" {
		return  nil, nil
	}

	replicaSet := &appsv1.ReplicaSet{}
	if err := t.kubeClient.Get(ctx,  client.ObjectKey{Name: rsName, Namespace: pod.Namespace},replicaSet); err != nil {
		return nil, fmt.Errorf("get rs, %w", err)
	}

	var dpName string
	for _, ownerRef := range replicaSet.GetOwnerReferences() {
		if *ownerRef.Controller && ownerRef.Kind == "Deployment" {
			dpName = ownerRef.Name
			break
		}
	}
	if dpName == "" {
		return  nil, nil
	}
	deployment := &appsv1.Deployment{}
	if err := t.kubeClient.Get(ctx,  client.ObjectKey{Name: dpName, Namespace: pod.Namespace},deployment); err != nil {
		return nil, fmt.Errorf("get deployment, %w", err)
	}

	return deployment, nil
}

func (t *Terminator) RestartDeployments(ctx context.Context, deployments []*appsv1.Deployment, nodeName string ) error {
	for _, deployment := range deployments {
		if deployment.Spec.Template.Annotations == nil {
			deployment.Spec.Template.Annotations = make(map[string]string)
		}
		restartedAt, exists := deployment.Spec.Template.Annotations["kubectl.kubernetes.io/restartedNode"]
		if exists && restartedAt == nodeName {
			continue
		}

		log.FromContext(ctx).V(1).
			WithValues("restart dp",deployment.Namespace).
			WithValues("name",deployment.Name).
			Info("#debug43")

		deployment.Spec.Template.Annotations["kubectl.kubernetes.io/restartedAt"] = nodeName
		if err := t.kubeClient.Update(ctx, deployment); err != nil {
			return err
		}

	}
	return nil
}

func (t *Terminator) GetdeploymentsAndDrainPodsFromNode(ctx context.Context, node *corev1.Node) ([]*appsv1.Deployment, []*corev1.Pod,error){
	nodePods, err := nodeutil.GetPods(ctx, t.kubeClient, node)
	if err != nil {
		return nil,nil,fmt.Errorf("listing pods on node, %w", err)
	}

	var drainPods []*corev1.Pod
	var restartDeployments []*appsv1.Deployment
	nodeDeploymentReplicas := make(map[string]int32)

	for _, pod := range nodePods {
		deployment, err := t.GetDeploymentFromPod(ctx, pod)
		if err != nil {
			return nil,nil,err
		}

		if deployment != nil {
			nodeDeploymentReplicas[deployment.Namespace+":"+deployment.Name] += 1
		}
	}

	data, _ := json.Marshal(&nodeDeploymentReplicas)
	log.FromContext(ctx).V(1).
		WithValues("nodeDeploymentReplicas",string(data)).
		WithValues("node",node.Name).
		Info("#debug51")


	for _, pod := range nodePods {
		deployment, err := t.GetDeploymentFromPod(ctx, pod)
		if err != nil {
			return nil,nil,err
		}

		if deployment != nil && nodeDeploymentReplicas[deployment.Namespace+":"+deployment.Name] == *deployment.Spec.Replicas{
			restartDeployments = append(restartDeployments, deployment)
		} else {
			drainPods = append(drainPods, pod)
		}
	}

	return restartDeployments, drainPods, nil
}

// Drain evicts pods from the node and returns true when all pods are evicted
// https://kubernetes.io/docs/concepts/architecture/nodes/#graceful-node-shutdown
func (t *Terminator) Drain(ctx context.Context, node *corev1.Node, drainPods []*corev1.Pod, nodeGracePeriodExpirationTime *time.Time) error {
	pods, err := nodeutil.GetPods(ctx, t.kubeClient, node)
	if err != nil {
		return fmt.Errorf("listing pods on node, %w", err)
	}

	podsToDelete := lo.Filter(drainPods, func(p *corev1.Pod, _ int) bool {
		log.FromContext(ctx).V(1).
			WithValues("pod",p.Name).
			WithValues("IsWaitingEviction",podutil.IsWaitingEviction(p, t.clock)).
			WithValues("!IsTerminating",!podutil.IsTerminating(p)).
			Info("#debug48")
		return podutil.IsWaitingEviction(p, t.clock) && !podutil.IsTerminating(p)
	})

	log.FromContext(ctx).V(1).
		WithValues("podsToDelete",len(podsToDelete)).
		Info("#debug46")
	for _,v := range podsToDelete {
		log.FromContext(ctx).V(1).
			WithValues("podsToDelete",v.Name).
			Info("#debug34")
	}


	if err = t.DeleteExpiringPods(ctx, podsToDelete, nodeGracePeriodExpirationTime); err != nil {
		return fmt.Errorf("deleting expiring pods, %w", err)
	}

	// evictablePods are pods that aren't yet terminating are eligible to have the eviction API called against them
	evictablePods := lo.Filter(drainPods, func(p *corev1.Pod, _ int) bool { return podutil.IsEvictable(p) })

	for _,v := range evictablePods {
		log.FromContext(ctx).V(1).
			WithValues("evictablePods",v.Name).
			Info("#debug35")
	}


	t.Evict(evictablePods)

	log.FromContext(ctx).V(1).
		WithValues("evictablePods",len(evictablePods)).
		Info("#debug36")

	// podsWaitingEvictionCount is the number of pods that either haven't had eviction called against them yet
	// or are still actively terminating and haven't exceeded their termination grace period yet
	podsWaitingEvictionCount := lo.CountBy(pods, func(p *corev1.Pod) bool {
		log.FromContext(ctx).V(1).
			WithValues("pod",p.Name).
			WithValues("IsWaitingEviction",podutil.IsWaitingEviction(p, t.clock)).
			WithValues("!IsTerminating",!podutil.IsTerminal(p)).
			WithValues("IsDrainable",podutil.IsDrainable(p, t.clock)).
			Info("#debug49")


		return podutil.IsWaitingEviction(p, t.clock)
	})
	if podsWaitingEvictionCount > 0 {
		return NewNodeDrainError(fmt.Errorf("%d pods are waiting to be evicted", pods))
	}
	return nil
}

func (t *Terminator) Evict(pods []*corev1.Pod) {
	// 1. Prioritize noncritical pods, non-daemon pods https://kubernetes.io/docs/concepts/architecture/nodes/#graceful-node-shutdown
	var criticalNonDaemon, criticalDaemon, nonCriticalNonDaemon, nonCriticalDaemon []*corev1.Pod
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

	// EvictInOrder evicts only the first list of pods which is not empty
	// future Evict calls will catch later lists of pods that were not initially evicted
	t.EvictInOrder(
		nonCriticalNonDaemon,
		nonCriticalDaemon,
		criticalNonDaemon,
		criticalDaemon,
	)
}

func (t *Terminator) EvictInOrder(pods ...[]*corev1.Pod) {
	for _, podList := range pods {
		if len(podList) > 0 {
			// evict the first list of pods that is not empty, ignore the rest
			t.evictionQueue.Add(podList...)
			return
		}
	}
}

func (t *Terminator) DeleteExpiringPods(ctx context.Context, pods []*corev1.Pod, nodeGracePeriodTerminationTime *time.Time) error {
	for _, pod := range pods {


		log.FromContext(ctx).V(1).
			WithValues("pod name", pod.Name).
			Info("#debug38")

		// check if the node has an expiration time and the pod needs to be deleted
		deleteTime := t.podDeleteTimeWithGracePeriod(nodeGracePeriodTerminationTime, pod)
		if deleteTime != nil && time.Now().After(*deleteTime) {

			log.FromContext(ctx).V(1).
				WithValues("pod name", pod.Name).
				Info("#debug39")
			// delete pod proactively to give as much of its terminationGracePeriodSeconds as possible for deletion
			// ensure that we clamp the maximum pod terminationGracePeriodSeconds to the node's remaining expiration time in the delete command
			gracePeriodSeconds := lo.ToPtr(int64(time.Until(*nodeGracePeriodTerminationTime).Seconds()))
			t.recorder.Publish(terminatorevents.DisruptPodDelete(pod, gracePeriodSeconds, nodeGracePeriodTerminationTime))
			opts := &client.DeleteOptions{
				GracePeriodSeconds: gracePeriodSeconds,
			}
			if err := t.kubeClient.Delete(ctx, pod, opts); err != nil && !apierrors.IsNotFound(err) { // ignore 404, not a problem
				return fmt.Errorf("deleting pod, %w", err) // otherwise, bubble up the error
			}

			log.FromContext(ctx).V(1).
				WithValues("pod name", pod.Name).
				Info("#debug40")

			log.FromContext(ctx).WithValues(
				"namespace", pod.Namespace,
				"name", pod.Name,
				"pod.terminationGracePeriodSeconds", *pod.Spec.TerminationGracePeriodSeconds,
				"delete.gracePeriodSeconds", *gracePeriodSeconds,
				"nodeclaim.terminationTime", *nodeGracePeriodTerminationTime,
			).V(1).Info("deleting pod")
		}
	}
	return nil
}

// if a pod should be deleted to give it the full terminationGracePeriodSeconds of time before the node will shut down, return the time the pod should be deleted
func (t *Terminator) podDeleteTimeWithGracePeriod(nodeGracePeriodExpirationTime *time.Time, pod *corev1.Pod) *time.Time {
	if nodeGracePeriodExpirationTime == nil || pod.Spec.TerminationGracePeriodSeconds == nil { // k8s defaults to 30s, so we should never see a nil TerminationGracePeriodSeconds
		return nil
	}

	// calculate the time the pod should be deleted to allow it's full grace period for termination, equal to its terminationGracePeriodSeconds before the node's expiration time
	// eg: if a node will be force terminated in 30m, but the current pod has a grace period of 45m, we return a time of 15m ago
	deleteTime := nodeGracePeriodExpirationTime.Add(time.Duration(*pod.Spec.TerminationGracePeriodSeconds) * time.Second * -1)
	return &deleteTime
}
