/*
Copyright 2016 The Kubernetes Authors.

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

package clusterstate

import (
	"fmt"
	"testing"
	"time"

	"k8s.io/autoscaler/cluster-autoscaler/metrics"

	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider"
	testprovider "k8s.io/autoscaler/cluster-autoscaler/cloudprovider/test"
	"k8s.io/autoscaler/cluster-autoscaler/clusterstate/api"
	"k8s.io/autoscaler/cluster-autoscaler/clusterstate/utils"
	"k8s.io/autoscaler/cluster-autoscaler/utils/deletetaint"
	. "k8s.io/autoscaler/cluster-autoscaler/utils/test"
	"k8s.io/client-go/kubernetes/fake"
	kube_record "k8s.io/client-go/tools/record"

	"github.com/stretchr/testify/assert"
	"k8s.io/autoscaler/cluster-autoscaler/utils/backoff"
)

// GetCloudProviderDeletedNodeNames returns a list of the names of nodes removed
// from cloud provider but registered in Kubernetes.
func GetCloudProviderDeletedNodeNames(csr *ClusterStateRegistry) []string {
	csr.Lock()
	defer csr.Unlock()

	result := make([]string, 0, len(csr.deletedNodes))
	for nodeName := range csr.deletedNodes {
		result = append(result, nodeName)
	}
	return result
}

func TestOKWithScaleUp(t *testing.T) {
	now := time.Now()

	ng1_1 := BuildTestNode("ng1-1", 1000, 1000)
	SetNodeReadyState(ng1_1, true, now.Add(-time.Minute))
	ng2_1 := BuildTestNode("ng2-1", 1000, 1000)
	SetNodeReadyState(ng2_1, true, now.Add(-time.Minute))

	provider := testprovider.NewTestCloudProvider(nil, nil)
	provider.AddNodeGroup("ng1", 1, 10, 5)
	provider.AddNodeGroup("ng2", 1, 10, 1)

	provider.AddNode("ng1", ng1_1)
	provider.AddNode("ng2", ng2_1)
	assert.NotNil(t, provider)

	fakeClient := &fake.Clientset{}
	fakeLogRecorder, _ := utils.NewStatusMapRecorder(fakeClient, "kube-system", kube_record.NewFakeRecorder(5), false, "my-cool-configmap")
	clusterstate := NewClusterStateRegistry(provider, ClusterStateRegistryConfig{
		MaxTotalUnreadyPercentage: 10,
		OkTotalUnreadyCount:       1,
		MaxNodeProvisionTime:      time.Minute,
	}, fakeLogRecorder, newBackoff())
	clusterstate.RegisterOrUpdateScaleUp(provider.GetNodeGroup("ng1"), 4, time.Now())
	err := clusterstate.UpdateNodes([]*apiv1.Node{ng1_1, ng2_1}, nil, now)
	assert.NoError(t, err)
	assert.True(t, clusterstate.IsClusterHealthy())
	assert.Empty(t, clusterstate.GetScaleUpFailures())

	status := clusterstate.GetStatus(now)
	assert.Equal(t, api.ClusterAutoscalerInProgress,
		api.GetConditionByType(api.ClusterAutoscalerScaleUp, status.ClusterwideConditions).Status)
	assert.Equal(t, 2, len(status.NodeGroupStatuses))
	ng1Checked := false
	ng2Checked := true
	for _, nodeStatus := range status.NodeGroupStatuses {
		if nodeStatus.ProviderID == "ng1" {
			assert.Equal(t, api.ClusterAutoscalerInProgress,
				api.GetConditionByType(api.ClusterAutoscalerScaleUp, nodeStatus.Conditions).Status)
			ng1Checked = true
		}
		if nodeStatus.ProviderID == "ng2" {
			assert.Equal(t, api.ClusterAutoscalerNoActivity,
				api.GetConditionByType(api.ClusterAutoscalerScaleUp, nodeStatus.Conditions).Status)
			ng2Checked = true
		}
	}
	assert.True(t, ng1Checked)
	assert.True(t, ng2Checked)
}

func TestEmptyOK(t *testing.T) {
	now := time.Now()

	provider := testprovider.NewTestCloudProvider(nil, nil)
	provider.AddNodeGroup("ng1", 0, 10, 0)
	assert.NotNil(t, provider)

	fakeClient := &fake.Clientset{}
	fakeLogRecorder, _ := utils.NewStatusMapRecorder(fakeClient, "kube-system", kube_record.NewFakeRecorder(5), false, "my-cool-configmap")
	clusterstate := NewClusterStateRegistry(provider, ClusterStateRegistryConfig{
		MaxTotalUnreadyPercentage: 10,
		OkTotalUnreadyCount:       1,
		MaxNodeProvisionTime:      time.Minute,
	}, fakeLogRecorder, newBackoff())
	err := clusterstate.UpdateNodes([]*apiv1.Node{}, nil, now.Add(-5*time.Second))
	assert.NoError(t, err)
	assert.True(t, clusterstate.IsClusterHealthy())
	assert.Empty(t, clusterstate.GetScaleUpFailures())
	assert.True(t, clusterstate.IsNodeGroupHealthy("ng1"))
	assert.False(t, clusterstate.IsNodeGroupScalingUp("ng1"))

	provider.AddNodeGroup("ng1", 0, 10, 3)
	clusterstate.RegisterOrUpdateScaleUp(provider.GetNodeGroup("ng1"), 3, now.Add(-3*time.Second))
	//	clusterstate.scaleUpRequests["ng1"].Time = now.Add(-3 * time.Second)
	//	clusterstate.scaleUpRequests["ng1"].ExpectedAddTime = now.Add(1 * time.Minute)
	err = clusterstate.UpdateNodes([]*apiv1.Node{}, nil, now)

	assert.NoError(t, err)
	assert.True(t, clusterstate.IsClusterHealthy())
	assert.True(t, clusterstate.IsNodeGroupHealthy("ng1"))
	assert.True(t, clusterstate.IsNodeGroupScalingUp("ng1"))
}

func TestOKOneUnreadyNode(t *testing.T) {
	now := time.Now()

	ng1_1 := BuildTestNode("ng1-1", 1000, 1000)
	SetNodeReadyState(ng1_1, true, now.Add(-time.Minute))
	ng2_1 := BuildTestNode("ng2-1", 1000, 1000)
	SetNodeReadyState(ng2_1, false, now.Add(-time.Minute))

	provider := testprovider.NewTestCloudProvider(nil, nil)
	provider.AddNodeGroup("ng1", 1, 10, 1)
	provider.AddNodeGroup("ng2", 1, 10, 1)
	provider.AddNode("ng1", ng1_1)
	provider.AddNode("ng2", ng2_1)
	assert.NotNil(t, provider)

	fakeClient := &fake.Clientset{}
	fakeLogRecorder, _ := utils.NewStatusMapRecorder(fakeClient, "kube-system", kube_record.NewFakeRecorder(5), false, "my-cool-configmap")
	clusterstate := NewClusterStateRegistry(provider, ClusterStateRegistryConfig{
		MaxTotalUnreadyPercentage: 10,
		OkTotalUnreadyCount:       1,
	}, fakeLogRecorder, newBackoff())
	err := clusterstate.UpdateNodes([]*apiv1.Node{ng1_1, ng2_1}, nil, now)
	assert.NoError(t, err)
	assert.True(t, clusterstate.IsClusterHealthy())
	assert.Empty(t, clusterstate.GetScaleUpFailures())
	assert.True(t, clusterstate.IsNodeGroupHealthy("ng1"))

	status := clusterstate.GetStatus(now)
	assert.Equal(t, api.ClusterAutoscalerHealthy,
		api.GetConditionByType(api.ClusterAutoscalerHealth, status.ClusterwideConditions).Status)
	assert.Equal(t, api.ClusterAutoscalerNoActivity,
		api.GetConditionByType(api.ClusterAutoscalerScaleUp, status.ClusterwideConditions).Status)

	assert.Equal(t, 2, len(status.NodeGroupStatuses))
	ng1Checked := false
	for _, nodeStatus := range status.NodeGroupStatuses {
		if nodeStatus.ProviderID == "ng1" {
			assert.Equal(t, api.ClusterAutoscalerHealthy,
				api.GetConditionByType(api.ClusterAutoscalerHealth, nodeStatus.Conditions).Status)
			ng1Checked = true
		}
	}
	assert.True(t, ng1Checked)
}

func TestNodeWithoutNodeGroupDontCrash(t *testing.T) {
	now := time.Now()

	noNgNode := BuildTestNode("no_ng", 1000, 1000)
	SetNodeReadyState(noNgNode, true, now.Add(-time.Minute))
	provider := testprovider.NewTestCloudProvider(nil, nil)
	provider.AddNode("no_ng", noNgNode)

	fakeClient := &fake.Clientset{}
	fakeLogRecorder, _ := utils.NewStatusMapRecorder(fakeClient, "kube-system", kube_record.NewFakeRecorder(5), false, "my-cool-configmap")
	clusterstate := NewClusterStateRegistry(provider, ClusterStateRegistryConfig{
		MaxTotalUnreadyPercentage: 10,
		OkTotalUnreadyCount:       1,
	}, fakeLogRecorder, newBackoff())
	err := clusterstate.UpdateNodes([]*apiv1.Node{noNgNode}, nil, now)
	assert.NoError(t, err)
	assert.Empty(t, clusterstate.GetScaleUpFailures())
	clusterstate.UpdateScaleDownCandidates([]*apiv1.Node{noNgNode}, now)
}

func TestOKOneUnreadyNodeWithScaleDownCandidate(t *testing.T) {
	now := time.Now()

	ng1_1 := BuildTestNode("ng1-1", 1000, 1000)
	SetNodeReadyState(ng1_1, true, now.Add(-time.Minute))
	ng2_1 := BuildTestNode("ng2-1", 1000, 1000)
	SetNodeReadyState(ng2_1, false, now.Add(-time.Minute))

	provider := testprovider.NewTestCloudProvider(nil, nil)
	provider.AddNodeGroup("ng1", 1, 10, 1)
	provider.AddNodeGroup("ng2", 1, 10, 1)
	provider.AddNode("ng1", ng1_1)
	provider.AddNode("ng2", ng2_1)
	assert.NotNil(t, provider)

	fakeClient := &fake.Clientset{}
	fakeLogRecorder, _ := utils.NewStatusMapRecorder(fakeClient, "kube-system", kube_record.NewFakeRecorder(5), false, "my-cool-configmap")
	clusterstate := NewClusterStateRegistry(provider, ClusterStateRegistryConfig{
		MaxTotalUnreadyPercentage: 10,
		OkTotalUnreadyCount:       1,
	}, fakeLogRecorder, newBackoff())
	err := clusterstate.UpdateNodes([]*apiv1.Node{ng1_1, ng2_1}, nil, now)
	clusterstate.UpdateScaleDownCandidates([]*apiv1.Node{ng1_1}, now)

	assert.NoError(t, err)
	assert.True(t, clusterstate.IsClusterHealthy())
	assert.Empty(t, clusterstate.GetScaleUpFailures())
	assert.True(t, clusterstate.IsNodeGroupHealthy("ng1"))

	status := clusterstate.GetStatus(now)
	assert.Equal(t, api.ClusterAutoscalerHealthy,
		api.GetConditionByType(api.ClusterAutoscalerHealth, status.ClusterwideConditions).Status)
	assert.Equal(t, api.ClusterAutoscalerNoActivity,
		api.GetConditionByType(api.ClusterAutoscalerScaleUp, status.ClusterwideConditions).Status)
	assert.Equal(t, api.ClusterAutoscalerCandidatesPresent,
		api.GetConditionByType(api.ClusterAutoscalerScaleDown, status.ClusterwideConditions).Status)

	assert.Equal(t, 2, len(status.NodeGroupStatuses))
	ng1Checked := false
	ng2Checked := false
	for _, nodeStatus := range status.NodeGroupStatuses {
		if nodeStatus.ProviderID == "ng1" {
			assert.Equal(t, api.ClusterAutoscalerHealthy,
				api.GetConditionByType(api.ClusterAutoscalerHealth, nodeStatus.Conditions).Status)

			assert.Equal(t, api.ClusterAutoscalerCandidatesPresent,
				api.GetConditionByType(api.ClusterAutoscalerScaleDown, nodeStatus.Conditions).Status)

			ng1Checked = true
		}
		if nodeStatus.ProviderID == "ng2" {
			assert.Equal(t, api.ClusterAutoscalerHealthy,
				api.GetConditionByType(api.ClusterAutoscalerHealth, nodeStatus.Conditions).Status)

			assert.Equal(t, api.ClusterAutoscalerNoCandidates,
				api.GetConditionByType(api.ClusterAutoscalerScaleDown, nodeStatus.Conditions).Status)

			ng2Checked = true
		}
	}
	assert.True(t, ng1Checked)
	assert.True(t, ng2Checked)
}

func TestMissingNodes(t *testing.T) {
	now := time.Now()

	ng1_1 := BuildTestNode("ng1-1", 1000, 1000)
	SetNodeReadyState(ng1_1, true, now.Add(-time.Minute))
	ng2_1 := BuildTestNode("ng2-1", 1000, 1000)
	SetNodeReadyState(ng2_1, true, now.Add(-time.Minute))

	provider := testprovider.NewTestCloudProvider(nil, nil)
	provider.AddNodeGroup("ng1", 1, 10, 5)
	provider.AddNodeGroup("ng2", 1, 10, 1)

	provider.AddNode("ng1", ng1_1)
	provider.AddNode("ng2", ng2_1)
	assert.NotNil(t, provider)

	fakeClient := &fake.Clientset{}
	fakeLogRecorder, _ := utils.NewStatusMapRecorder(fakeClient, "kube-system", kube_record.NewFakeRecorder(5), false, "my-cool-configmap")
	clusterstate := NewClusterStateRegistry(provider, ClusterStateRegistryConfig{
		MaxTotalUnreadyPercentage: 10,
		OkTotalUnreadyCount:       1,
	}, fakeLogRecorder, newBackoff())
	err := clusterstate.UpdateNodes([]*apiv1.Node{ng1_1, ng2_1}, nil, now)
	assert.NoError(t, err)
	assert.True(t, clusterstate.IsClusterHealthy())
	assert.Empty(t, clusterstate.GetScaleUpFailures())
	assert.False(t, clusterstate.IsNodeGroupHealthy("ng1"))

	status := clusterstate.GetStatus(now)
	assert.Equal(t, api.ClusterAutoscalerHealthy,
		api.GetConditionByType(api.ClusterAutoscalerHealth, status.ClusterwideConditions).Status)
	assert.Equal(t, 2, len(status.NodeGroupStatuses))
	ng1Checked := false
	for _, nodeStatus := range status.NodeGroupStatuses {
		if nodeStatus.ProviderID == "ng1" {
			assert.Equal(t, api.ClusterAutoscalerUnhealthy,
				api.GetConditionByType(api.ClusterAutoscalerHealth, nodeStatus.Conditions).Status)
			ng1Checked = true
		}
	}
	assert.True(t, ng1Checked)
}

func TestTooManyUnready(t *testing.T) {
	now := time.Now()

	ng1_1 := BuildTestNode("ng1-1", 1000, 1000)
	SetNodeReadyState(ng1_1, false, now.Add(-time.Minute))
	ng2_1 := BuildTestNode("ng2-1", 1000, 1000)
	SetNodeReadyState(ng2_1, false, now.Add(-time.Minute))

	provider := testprovider.NewTestCloudProvider(nil, nil)
	provider.AddNodeGroup("ng1", 1, 10, 1)
	provider.AddNodeGroup("ng2", 1, 10, 1)
	provider.AddNode("ng1", ng1_1)
	provider.AddNode("ng2", ng2_1)

	assert.NotNil(t, provider)
	fakeClient := &fake.Clientset{}
	fakeLogRecorder, _ := utils.NewStatusMapRecorder(fakeClient, "kube-system", kube_record.NewFakeRecorder(5), false, "my-cool-configmap")
	clusterstate := NewClusterStateRegistry(provider, ClusterStateRegistryConfig{
		MaxTotalUnreadyPercentage: 10,
		OkTotalUnreadyCount:       1,
	}, fakeLogRecorder, newBackoff())
	err := clusterstate.UpdateNodes([]*apiv1.Node{ng1_1, ng2_1}, nil, now)
	assert.NoError(t, err)
	assert.False(t, clusterstate.IsClusterHealthy())
	assert.Empty(t, clusterstate.GetScaleUpFailures())
	assert.True(t, clusterstate.IsNodeGroupHealthy("ng1"))
}

func TestUnreadyLongAfterCreation(t *testing.T) {
	now := time.Now()

	ng1_1 := BuildTestNode("ng1-1", 1000, 1000)
	SetNodeReadyState(ng1_1, true, now.Add(-time.Minute))
	ng2_1 := BuildTestNode("ng2-1", 1000, 1000)
	SetNodeReadyState(ng2_1, false, now.Add(-time.Minute))
	ng2_1.CreationTimestamp = metav1.Time{Time: now.Add(-30 * time.Minute)}

	provider := testprovider.NewTestCloudProvider(nil, nil)
	provider.AddNodeGroup("ng1", 1, 10, 1)
	provider.AddNodeGroup("ng2", 1, 10, 1)
	provider.AddNode("ng1", ng1_1)
	provider.AddNode("ng2", ng2_1)

	assert.NotNil(t, provider)
	fakeClient := &fake.Clientset{}
	fakeLogRecorder, _ := utils.NewStatusMapRecorder(fakeClient, "kube-system", kube_record.NewFakeRecorder(5), false, "some-map")
	clusterstate := NewClusterStateRegistry(provider, ClusterStateRegistryConfig{
		MaxTotalUnreadyPercentage: 10,
		OkTotalUnreadyCount:       1,
	}, fakeLogRecorder, newBackoff())
	err := clusterstate.UpdateNodes([]*apiv1.Node{ng1_1, ng2_1}, nil, now)
	assert.NoError(t, err)
	assert.Equal(t, 1, len(clusterstate.GetClusterReadiness().Unready))
	assert.Equal(t, 0, len(clusterstate.GetClusterReadiness().NotStarted))
	upcoming, upcomingRegistered := clusterstate.GetUpcomingNodes()
	assert.Equal(t, 0, upcoming["ng1"])
	assert.Empty(t, upcomingRegistered["ng1"])
}

func TestNotStarted(t *testing.T) {
	now := time.Now()

	ng1_1 := BuildTestNode("ng1-1", 1000, 1000)
	SetNodeReadyState(ng1_1, true, now.Add(-time.Minute))
	ng2_1 := BuildTestNode("ng2-1", 1000, 1000)
	SetNodeReadyState(ng2_1, false, now.Add(-4*time.Minute))
	SetNodeNotReadyTaint(ng2_1)
	ng2_1.CreationTimestamp = metav1.Time{Time: now.Add(-10 * time.Minute)}

	provider := testprovider.NewTestCloudProvider(nil, nil)
	provider.AddNodeGroup("ng1", 1, 10, 1)
	provider.AddNodeGroup("ng2", 1, 10, 1)
	provider.AddNode("ng1", ng1_1)
	provider.AddNode("ng2", ng2_1)

	assert.NotNil(t, provider)
	fakeClient := &fake.Clientset{}
	fakeLogRecorder, _ := utils.NewStatusMapRecorder(fakeClient, "kube-system", kube_record.NewFakeRecorder(5), false, "some-map")
	clusterstate := NewClusterStateRegistry(provider, ClusterStateRegistryConfig{
		MaxTotalUnreadyPercentage: 10,
		OkTotalUnreadyCount:       1,
	}, fakeLogRecorder, newBackoff())
	err := clusterstate.UpdateNodes([]*apiv1.Node{ng1_1, ng2_1}, nil, now)
	assert.NoError(t, err)
	assert.Equal(t, 1, len(clusterstate.GetClusterReadiness().NotStarted))
	assert.Equal(t, 1, len(clusterstate.GetClusterReadiness().Ready))

	// node ng2_1 moves condition to ready
	SetNodeReadyState(ng2_1, true, now.Add(-4*time.Minute))
	err = clusterstate.UpdateNodes([]*apiv1.Node{ng1_1, ng2_1}, nil, now)
	assert.NoError(t, err)
	assert.Equal(t, 1, len(clusterstate.GetClusterReadiness().NotStarted))
	assert.Equal(t, 1, len(clusterstate.GetClusterReadiness().Ready))

	// node ng2_1 no longer has the taint
	RemoveNodeNotReadyTaint(ng2_1)
	err = clusterstate.UpdateNodes([]*apiv1.Node{ng1_1, ng2_1}, nil, now)
	assert.NoError(t, err)
	assert.Equal(t, 0, len(clusterstate.GetClusterReadiness().NotStarted))
	assert.Equal(t, 2, len(clusterstate.GetClusterReadiness().Ready))
}

func TestExpiredScaleUp(t *testing.T) {
	now := time.Now()

	ng1_1 := BuildTestNode("ng1-1", 1000, 1000)
	SetNodeReadyState(ng1_1, true, now.Add(-time.Minute))

	provider := testprovider.NewTestCloudProvider(nil, nil)
	provider.AddNodeGroup("ng1", 1, 10, 5)
	provider.AddNode("ng1", ng1_1)
	assert.NotNil(t, provider)

	fakeClient := &fake.Clientset{}
	fakeLogRecorder, _ := utils.NewStatusMapRecorder(fakeClient, "kube-system", kube_record.NewFakeRecorder(5), false, "my-cool-configmap")
	clusterstate := NewClusterStateRegistry(provider, ClusterStateRegistryConfig{
		MaxTotalUnreadyPercentage: 10,
		OkTotalUnreadyCount:       1,
		MaxNodeProvisionTime:      2 * time.Minute,
	}, fakeLogRecorder, newBackoff())
	clusterstate.RegisterOrUpdateScaleUp(provider.GetNodeGroup("ng1"), 4, now.Add(-3*time.Minute))
	err := clusterstate.UpdateNodes([]*apiv1.Node{ng1_1}, nil, now)
	assert.NoError(t, err)
	assert.True(t, clusterstate.IsClusterHealthy())
	assert.False(t, clusterstate.IsNodeGroupHealthy("ng1"))
	assert.Equal(t, clusterstate.GetScaleUpFailures(), map[string][]ScaleUpFailure{
		"ng1": {
			{NodeGroup: provider.GetNodeGroup("ng1"), Time: now, Reason: metrics.Timeout},
		},
	})
}

func TestRegisterScaleDown(t *testing.T) {
	ng1_1 := BuildTestNode("ng1-1", 1000, 1000)
	provider := testprovider.NewTestCloudProvider(nil, nil)
	provider.AddNodeGroup("ng1", 1, 10, 1)
	provider.AddNode("ng1", ng1_1)
	assert.NotNil(t, provider)

	fakeClient := &fake.Clientset{}
	fakeLogRecorder, _ := utils.NewStatusMapRecorder(fakeClient, "kube-system", kube_record.NewFakeRecorder(5), false, "my-cool-configmap")
	clusterstate := NewClusterStateRegistry(provider, ClusterStateRegistryConfig{
		MaxTotalUnreadyPercentage: 10,
		OkTotalUnreadyCount:       1,
	}, fakeLogRecorder, newBackoff())

	now := time.Now()

	clusterstate.RegisterScaleDown(&ScaleDownRequest{
		NodeGroup:          provider.GetNodeGroup("ng1"),
		NodeName:           "ng1-1",
		ExpectedDeleteTime: now.Add(time.Minute),
		Time:               now,
	})
	assert.Equal(t, 1, len(clusterstate.scaleDownRequests))
	clusterstate.updateScaleRequests(now.Add(5 * time.Minute))
	assert.Equal(t, 0, len(clusterstate.scaleDownRequests))
	assert.Empty(t, clusterstate.GetScaleUpFailures())
}

func TestUpcomingNodes(t *testing.T) {
	provider := testprovider.NewTestCloudProvider(nil, nil)
	now := time.Now()

	// 6 nodes are expected to come.
	ng1_1 := BuildTestNode("ng1-1", 1000, 1000)
	SetNodeReadyState(ng1_1, true, now.Add(-time.Minute))
	provider.AddNodeGroup("ng1", 1, 10, 7)
	provider.AddNode("ng1", ng1_1)

	// One node is expected to come. One node is unready for the long time
	// but this should not make any difference.
	ng2_1 := BuildTestNode("ng2-1", 1000, 1000)
	SetNodeReadyState(ng2_1, false, now.Add(-time.Minute))
	provider.AddNodeGroup("ng2", 1, 10, 2)
	provider.AddNode("ng2", ng2_1)

	// Two nodes are expected to come. One is just being started for the first time,
	// the other one is not there yet.
	ng3_1 := BuildTestNode("ng3-1", 1000, 1000)
	SetNodeReadyState(ng3_1, false, now.Add(-time.Minute))
	ng3_1.CreationTimestamp = metav1.Time{Time: now.Add(-time.Minute)}
	provider.AddNodeGroup("ng3", 1, 10, 2)
	provider.AddNode("ng3", ng3_1)

	// Nothing should be added here.
	ng4_1 := BuildTestNode("ng4-1", 1000, 1000)
	SetNodeReadyState(ng4_1, false, now.Add(-time.Minute))
	provider.AddNodeGroup("ng4", 1, 10, 1)
	provider.AddNode("ng4", ng4_1)

	// One node is already there, for a second nde deletion / draining was already started.
	ng5_1 := BuildTestNode("ng5-1", 1000, 1000)
	SetNodeReadyState(ng5_1, true, now.Add(-time.Minute))
	ng5_2 := BuildTestNode("ng5-2", 1000, 1000)
	SetNodeReadyState(ng5_2, true, now.Add(-time.Minute))
	ng5_2.Spec.Taints = []apiv1.Taint{
		{
			Key:    deletetaint.ToBeDeletedTaint,
			Value:  fmt.Sprint(time.Now().Unix()),
			Effect: apiv1.TaintEffectNoSchedule,
		},
	}
	provider.AddNodeGroup("ng5", 1, 10, 2)
	provider.AddNode("ng5", ng5_1)
	provider.AddNode("ng5", ng5_2)

	assert.NotNil(t, provider)
	fakeClient := &fake.Clientset{}
	fakeLogRecorder, _ := utils.NewStatusMapRecorder(fakeClient, "kube-system", kube_record.NewFakeRecorder(5), false, "my-cool-configmap")
	clusterstate := NewClusterStateRegistry(provider, ClusterStateRegistryConfig{
		MaxTotalUnreadyPercentage: 10,
		OkTotalUnreadyCount:       1,
	}, fakeLogRecorder, newBackoff())
	err := clusterstate.UpdateNodes([]*apiv1.Node{ng1_1, ng2_1, ng3_1, ng4_1, ng5_1, ng5_2}, nil, now)
	assert.NoError(t, err)
	assert.Empty(t, clusterstate.GetScaleUpFailures())

	upcomingNodes, upcomingRegistered := clusterstate.GetUpcomingNodes()
	assert.Equal(t, 6, upcomingNodes["ng1"])
	assert.Empty(t, upcomingRegistered["ng1"]) // Only unregistered.
	assert.Equal(t, 1, upcomingNodes["ng2"])
	assert.Empty(t, upcomingRegistered["ng2"]) // Only unregistered.
	assert.Equal(t, 2, upcomingNodes["ng3"])
	assert.Equal(t, []string{"ng3-1"}, upcomingRegistered["ng3"]) // 1 registered, 1 unregistered.
	assert.NotContains(t, upcomingNodes, "ng4")
	assert.NotContains(t, upcomingRegistered, "ng4")
	assert.Equal(t, 0, upcomingNodes["ng5"])
	assert.Empty(t, upcomingRegistered["ng5"])
}

func TestTaintBasedNodeDeletion(t *testing.T) {
	// Create a new Cloud Provider that does not implement the HasInstance check
	// it will return the ErrNotImplemented error instead.
	provider := testprovider.NewTestNodeDeletionDetectionCloudProvider(nil, nil,
		func(string) (bool, error) { return false, cloudprovider.ErrNotImplemented })
	now := time.Now()

	// One node is already there, for a second nde deletion / draining was already started.
	ng1_1 := BuildTestNode("ng1-1", 1000, 1000)
	SetNodeReadyState(ng1_1, true, now.Add(-time.Minute))
	ng1_2 := BuildTestNode("ng1-2", 1000, 1000)
	SetNodeReadyState(ng1_2, true, now.Add(-time.Minute))
	ng1_2.Spec.Taints = []apiv1.Taint{
		{
			Key:    deletetaint.ToBeDeletedTaint,
			Value:  fmt.Sprint(time.Now().Unix()),
			Effect: apiv1.TaintEffectNoSchedule,
		},
	}
	provider.AddNodeGroup("ng1", 1, 10, 2)
	provider.AddNode("ng1", ng1_1)
	provider.AddNode("ng1", ng1_2)

	assert.NotNil(t, provider)
	fakeClient := &fake.Clientset{}
	fakeLogRecorder, _ := utils.NewStatusMapRecorder(fakeClient, "kube-system", kube_record.NewFakeRecorder(5), false, "my-cool-configmap")
	clusterstate := NewClusterStateRegistry(provider, ClusterStateRegistryConfig{
		MaxTotalUnreadyPercentage: 10,
		OkTotalUnreadyCount:       1,
	}, fakeLogRecorder, newBackoff())
	err := clusterstate.UpdateNodes([]*apiv1.Node{ng1_1, ng1_2}, nil, now)
	assert.NoError(t, err)
	assert.Empty(t, clusterstate.GetScaleUpFailures())

	upcomingNodes, upcomingRegistered := clusterstate.GetUpcomingNodes()
	assert.Equal(t, 1, upcomingNodes["ng1"])
	assert.Empty(t, upcomingRegistered["ng1"]) // Only unregistered.
}

func TestIncorrectSize(t *testing.T) {
	ng1_1 := BuildTestNode("ng1-1", 1000, 1000)
	provider := testprovider.NewTestCloudProvider(nil, nil)
	provider.AddNodeGroup("ng1", 1, 10, 5)
	provider.AddNode("ng1", ng1_1)
	assert.NotNil(t, provider)
	fakeClient := &fake.Clientset{}
	fakeLogRecorder, _ := utils.NewStatusMapRecorder(fakeClient, "kube-system", kube_record.NewFakeRecorder(5), false, "my-cool-configmap")
	clusterstate := NewClusterStateRegistry(provider, ClusterStateRegistryConfig{
		MaxTotalUnreadyPercentage: 10,
		OkTotalUnreadyCount:       1,
	}, fakeLogRecorder, newBackoff())
	now := time.Now()
	clusterstate.UpdateNodes([]*apiv1.Node{ng1_1}, nil, now.Add(-5*time.Minute))
	incorrect := clusterstate.incorrectNodeGroupSizes["ng1"]
	assert.Equal(t, 5, incorrect.ExpectedSize)
	assert.Equal(t, 1, incorrect.CurrentSize)
	assert.Equal(t, now.Add(-5*time.Minute), incorrect.FirstObserved)

	clusterstate.UpdateNodes([]*apiv1.Node{ng1_1}, nil, now.Add(-4*time.Minute))
	incorrect = clusterstate.incorrectNodeGroupSizes["ng1"]
	assert.Equal(t, 5, incorrect.ExpectedSize)
	assert.Equal(t, 1, incorrect.CurrentSize)
	assert.Equal(t, now.Add(-5*time.Minute), incorrect.FirstObserved)

	clusterstate.UpdateNodes([]*apiv1.Node{ng1_1, ng1_1}, nil, now.Add(-3*time.Minute))
	incorrect = clusterstate.incorrectNodeGroupSizes["ng1"]
	assert.Equal(t, 5, incorrect.ExpectedSize)
	assert.Equal(t, 2, incorrect.CurrentSize)
	assert.Equal(t, now.Add(-3*time.Minute), incorrect.FirstObserved)
}

func TestUnregisteredNodes(t *testing.T) {
	ng1_1 := BuildTestNode("ng1-1", 1000, 1000)
	ng1_1.Spec.ProviderID = "ng1-1"
	ng1_2 := BuildTestNode("ng1-2", 1000, 1000)
	ng1_2.Spec.ProviderID = "ng1-2"
	provider := testprovider.NewTestCloudProvider(nil, nil)
	provider.AddNodeGroup("ng1", 1, 10, 2)
	provider.AddNode("ng1", ng1_1)
	provider.AddNode("ng1", ng1_2)

	fakeClient := &fake.Clientset{}
	fakeLogRecorder, _ := utils.NewStatusMapRecorder(fakeClient, "kube-system", kube_record.NewFakeRecorder(5), false, "my-cool-configmap")
	clusterstate := NewClusterStateRegistry(provider, ClusterStateRegistryConfig{
		MaxTotalUnreadyPercentage: 10,
		OkTotalUnreadyCount:       1,
		MaxNodeProvisionTime:      10 * time.Second,
	}, fakeLogRecorder, newBackoff())
	err := clusterstate.UpdateNodes([]*apiv1.Node{ng1_1}, nil, time.Now().Add(-time.Minute))

	assert.NoError(t, err)
	assert.Equal(t, 1, len(clusterstate.GetUnregisteredNodes()))
	assert.Equal(t, "ng1-2", clusterstate.GetUnregisteredNodes()[0].Node.Name)
	upcomingNodes, upcomingRegistered := clusterstate.GetUpcomingNodes()
	assert.Equal(t, 1, upcomingNodes["ng1"])
	assert.Empty(t, upcomingRegistered["ng1"]) // Unregistered only.

	// The node didn't come up in MaxNodeProvisionTime, it should no longer be
	// counted as upcoming (but it is still an unregistered node)
	err = clusterstate.UpdateNodes([]*apiv1.Node{ng1_1}, nil, time.Now().Add(time.Minute))
	assert.NoError(t, err)
	assert.Equal(t, 1, len(clusterstate.GetUnregisteredNodes()))
	assert.Equal(t, "ng1-2", clusterstate.GetUnregisteredNodes()[0].Node.Name)
	upcomingNodes, upcomingRegistered = clusterstate.GetUpcomingNodes()
	assert.Equal(t, 0, len(upcomingNodes))
	assert.Empty(t, upcomingRegistered["ng1"])

	err = clusterstate.UpdateNodes([]*apiv1.Node{ng1_1, ng1_2}, nil, time.Now().Add(time.Minute))
	assert.NoError(t, err)
	assert.Equal(t, 0, len(clusterstate.GetUnregisteredNodes()))
}

func TestCloudProviderDeletedNodes(t *testing.T) {
	now := time.Now()
	ng1_1 := BuildTestNode("ng1-1", 1000, 1000)
	SetNodeReadyState(ng1_1, true, now.Add(-time.Minute))
	ng1_1.Spec.ProviderID = "ng1-1"
	ng1_2 := BuildTestNode("ng1-2", 1000, 1000)
	SetNodeReadyState(ng1_2, true, now.Add(-time.Minute))
	ng1_2.Spec.ProviderID = "ng1-2"
	// No Node Group - Not Autoscaled Node
	noNgNode := BuildTestNode("no-ng", 1000, 1000)
	SetNodeReadyState(noNgNode, true, now.Add(-time.Minute))

	noNgNode.Spec.ProviderID = "no-ng"
	provider := testprovider.NewTestCloudProvider(nil, nil)
	provider.AddNodeGroup("ng1", 1, 10, 2)
	provider.AddNode("ng1", ng1_1)
	provider.AddNode("ng1", ng1_2)
	provider.AddNode("no_ng", noNgNode)

	fakeClient := &fake.Clientset{}
	fakeLogRecorder, _ := utils.NewStatusMapRecorder(fakeClient, "kube-system", kube_record.NewFakeRecorder(5), false, "my-cool-configmap")
	clusterstate := NewClusterStateRegistry(provider, ClusterStateRegistryConfig{
		MaxTotalUnreadyPercentage: 10,
		OkTotalUnreadyCount:       1,
		MaxNodeProvisionTime:      10 * time.Second,
	}, fakeLogRecorder, newBackoff())
	now.Add(time.Minute)
	err := clusterstate.UpdateNodes([]*apiv1.Node{ng1_1, ng1_2, noNgNode}, nil, now)

	// Nodes are registered correctly between Kubernetes and cloud provider.
	assert.NoError(t, err)
	assert.Equal(t, 0, len(GetCloudProviderDeletedNodeNames(clusterstate)))

	// The node was removed from Cloud Provider
	// should be counted as Deleted by cluster state
	nodeGroup, err := provider.NodeGroupForNode(ng1_2)
	assert.NoError(t, err)
	provider.DeleteNode(ng1_2)
	clusterstate.InvalidateNodeInstancesCacheEntry(nodeGroup)
	now.Add(time.Minute)

	err = clusterstate.UpdateNodes([]*apiv1.Node{ng1_1, ng1_2, noNgNode}, nil, now)
	assert.NoError(t, err)
	assert.Equal(t, 1, len(GetCloudProviderDeletedNodeNames(clusterstate)))
	assert.Equal(t, "ng1-2", GetCloudProviderDeletedNodeNames(clusterstate)[0])
	assert.Equal(t, 1, len(clusterstate.GetClusterReadiness().Deleted))

	// The node is removed from Kubernetes
	now.Add(time.Minute)

	err = clusterstate.UpdateNodes([]*apiv1.Node{ng1_1, noNgNode}, nil, now)
	assert.NoError(t, err)
	assert.Equal(t, 0, len(GetCloudProviderDeletedNodeNames(clusterstate)))

	// New Node is added afterwards
	ng1_3 := BuildTestNode("ng1-3", 1000, 1000)
	SetNodeReadyState(ng1_3, true, now.Add(-time.Minute))
	ng1_3.Spec.ProviderID = "ng1-3"
	provider.AddNode("ng1", ng1_3)
	clusterstate.InvalidateNodeInstancesCacheEntry(nodeGroup)
	now.Add(time.Minute)

	err = clusterstate.UpdateNodes([]*apiv1.Node{ng1_1, ng1_3, noNgNode}, nil, now)
	assert.NoError(t, err)
	assert.Equal(t, 0, len(GetCloudProviderDeletedNodeNames(clusterstate)))

	// Newly added node is removed from Cloud Provider
	// should be counted as Deleted by cluster state
	nodeGroup, err = provider.NodeGroupForNode(ng1_3)
	assert.NoError(t, err)
	provider.DeleteNode(ng1_3)
	clusterstate.InvalidateNodeInstancesCacheEntry(nodeGroup)
	now.Add(time.Minute)

	err = clusterstate.UpdateNodes([]*apiv1.Node{ng1_1, noNgNode, ng1_3}, nil, now)
	assert.NoError(t, err)
	assert.Equal(t, 1, len(GetCloudProviderDeletedNodeNames(clusterstate)))
	assert.Equal(t, "ng1-3", GetCloudProviderDeletedNodeNames(clusterstate)[0])
	assert.Equal(t, 1, len(clusterstate.GetClusterReadiness().Deleted))

	// Confirm that previously identified deleted Cloud Provider nodes are still included
	// until it is removed from Kubernetes
	now.Add(time.Minute)

	err = clusterstate.UpdateNodes([]*apiv1.Node{ng1_1, noNgNode, ng1_3}, nil, now)
	assert.NoError(t, err)
	assert.Equal(t, 1, len(GetCloudProviderDeletedNodeNames(clusterstate)))
	assert.Equal(t, "ng1-3", GetCloudProviderDeletedNodeNames(clusterstate)[0])
	assert.Equal(t, 1, len(clusterstate.GetClusterReadiness().Deleted))

	// The node is removed from Kubernetes
	now.Add(time.Minute)

	err = clusterstate.UpdateNodes([]*apiv1.Node{ng1_1, noNgNode}, nil, now)
	assert.NoError(t, err)
	assert.Equal(t, 0, len(GetCloudProviderDeletedNodeNames(clusterstate)))
}

func TestUpdateLastTransitionTimes(t *testing.T) {
	now := metav1.Time{Time: time.Now()}
	later := metav1.Time{Time: now.Time.Add(10 * time.Second)}
	oldStatus := &api.ClusterAutoscalerStatus{
		ClusterwideConditions: make([]api.ClusterAutoscalerCondition, 0),
		NodeGroupStatuses:     make([]api.NodeGroupStatus, 0),
	}
	oldStatus.ClusterwideConditions = append(
		oldStatus.ClusterwideConditions,
		api.ClusterAutoscalerCondition{
			Type:               api.ClusterAutoscalerHealth,
			Status:             api.ClusterAutoscalerHealthy,
			LastProbeTime:      now,
			LastTransitionTime: now,
		})
	oldStatus.ClusterwideConditions = append(
		oldStatus.ClusterwideConditions,
		api.ClusterAutoscalerCondition{
			Type:               api.ClusterAutoscalerScaleUp,
			Status:             api.ClusterAutoscalerInProgress,
			LastProbeTime:      now,
			LastTransitionTime: now,
		})
	oldStatus.NodeGroupStatuses = append(
		oldStatus.NodeGroupStatuses,
		api.NodeGroupStatus{
			ProviderID: "ng1",
			Conditions: oldStatus.ClusterwideConditions,
		})

	newStatus := &api.ClusterAutoscalerStatus{
		ClusterwideConditions: make([]api.ClusterAutoscalerCondition, 0),
		NodeGroupStatuses:     make([]api.NodeGroupStatus, 0),
	}
	newStatus.ClusterwideConditions = append(
		newStatus.ClusterwideConditions,
		api.ClusterAutoscalerCondition{
			Type:          api.ClusterAutoscalerHealth,
			Status:        api.ClusterAutoscalerHealthy,
			LastProbeTime: later,
		})
	newStatus.ClusterwideConditions = append(
		newStatus.ClusterwideConditions,
		api.ClusterAutoscalerCondition{
			Type:          api.ClusterAutoscalerScaleUp,
			Status:        api.ClusterAutoscalerNotNeeded,
			LastProbeTime: later,
		})
	newStatus.ClusterwideConditions = append(
		newStatus.ClusterwideConditions,
		api.ClusterAutoscalerCondition{
			Type:          api.ClusterAutoscalerScaleDown,
			Status:        api.ClusterAutoscalerNoCandidates,
			LastProbeTime: later,
		})
	newStatus.NodeGroupStatuses = append(
		newStatus.NodeGroupStatuses,
		api.NodeGroupStatus{
			ProviderID: "ng2",
			Conditions: newStatus.ClusterwideConditions,
		})
	newStatus.NodeGroupStatuses = append(
		newStatus.NodeGroupStatuses,
		api.NodeGroupStatus{
			ProviderID: "ng1",
			Conditions: newStatus.ClusterwideConditions,
		})
	updateLastTransition(oldStatus, newStatus)

	for _, cwCondition := range newStatus.ClusterwideConditions {
		switch cwCondition.Type {
		case api.ClusterAutoscalerHealth:
			// Status has not changed
			assert.Equal(t, now, cwCondition.LastTransitionTime)
		case api.ClusterAutoscalerScaleUp:
			// Status has changed
			assert.Equal(t, later, cwCondition.LastTransitionTime)
		case api.ClusterAutoscalerScaleDown:
			// No old status information
			assert.Equal(t, later, cwCondition.LastTransitionTime)
		}
	}

	expectedNgTimestamps := make(map[string]map[api.ClusterAutoscalerConditionType]metav1.Time, 0)
	// Same as cluster-wide
	expectedNgTimestamps["ng1"] = map[api.ClusterAutoscalerConditionType]metav1.Time{
		api.ClusterAutoscalerHealth:    now,
		api.ClusterAutoscalerScaleUp:   later,
		api.ClusterAutoscalerScaleDown: later,
	}
	// New node group - everything should have latest timestamp as last transition time
	expectedNgTimestamps["ng2"] = map[api.ClusterAutoscalerConditionType]metav1.Time{
		api.ClusterAutoscalerHealth:    later,
		api.ClusterAutoscalerScaleUp:   later,
		api.ClusterAutoscalerScaleDown: later,
	}

	for _, ng := range newStatus.NodeGroupStatuses {
		expectations := expectedNgTimestamps[ng.ProviderID]
		for _, ngCondition := range ng.Conditions {
			assert.Equal(t, expectations[ngCondition.Type], ngCondition.LastTransitionTime)
		}
	}
}

func TestScaleUpBackoff(t *testing.T) {
	now := time.Now()

	ng1_1 := BuildTestNode("ng1-1", 1000, 1000)
	SetNodeReadyState(ng1_1, true, now.Add(-time.Minute))
	ng1_2 := BuildTestNode("ng1-2", 1000, 1000)
	SetNodeReadyState(ng1_2, true, now.Add(-time.Minute))
	ng1_3 := BuildTestNode("ng1-3", 1000, 1000)
	SetNodeReadyState(ng1_3, true, now.Add(-time.Minute))

	provider := testprovider.NewTestCloudProvider(nil, nil)
	provider.AddNodeGroup("ng1", 1, 10, 4)
	ng1 := provider.GetNodeGroup("ng1")
	provider.AddNode("ng1", ng1_1)
	provider.AddNode("ng1", ng1_2)
	provider.AddNode("ng1", ng1_3)
	assert.NotNil(t, provider)

	fakeClient := &fake.Clientset{}
	fakeLogRecorder, _ := utils.NewStatusMapRecorder(fakeClient, "kube-system", kube_record.NewFakeRecorder(5), false, "my-cool-configmap")
	clusterstate := NewClusterStateRegistry(provider, ClusterStateRegistryConfig{
		MaxTotalUnreadyPercentage: 10,
		OkTotalUnreadyCount:       1,
		MaxNodeProvisionTime:      120 * time.Second,
	}, fakeLogRecorder, newBackoff())

	// After failed scale-up, node group should be still healthy, but should backoff from scale-ups
	clusterstate.RegisterOrUpdateScaleUp(provider.GetNodeGroup("ng1"), 1, now.Add(-180*time.Second))
	err := clusterstate.UpdateNodes([]*apiv1.Node{ng1_1, ng1_2, ng1_3}, nil, now)
	assert.NoError(t, err)
	assert.True(t, clusterstate.IsClusterHealthy())
	assert.True(t, clusterstate.IsNodeGroupHealthy("ng1"))
	assert.False(t, clusterstate.IsNodeGroupSafeToScaleUp(ng1, now))

	// Backoff should expire after timeout
	now = now.Add(5 * time.Minute /*InitialNodeGroupBackoffDuration*/).Add(time.Second)
	assert.True(t, clusterstate.IsClusterHealthy())
	assert.True(t, clusterstate.IsNodeGroupHealthy("ng1"))
	assert.True(t, clusterstate.IsNodeGroupSafeToScaleUp(ng1, now))

	// Another failed scale up should cause longer backoff
	clusterstate.RegisterOrUpdateScaleUp(provider.GetNodeGroup("ng1"), 1, now.Add(-121*time.Second))

	err = clusterstate.UpdateNodes([]*apiv1.Node{ng1_1, ng1_2, ng1_3}, nil, now)
	assert.NoError(t, err)
	assert.True(t, clusterstate.IsClusterHealthy())
	assert.True(t, clusterstate.IsNodeGroupHealthy("ng1"))
	assert.False(t, clusterstate.IsNodeGroupSafeToScaleUp(ng1, now))

	now = now.Add(5 * time.Minute /*InitialNodeGroupBackoffDuration*/).Add(time.Second)
	assert.False(t, clusterstate.IsNodeGroupSafeToScaleUp(ng1, now))

	// The backoff should be cleared after a successful scale-up
	clusterstate.RegisterOrUpdateScaleUp(provider.GetNodeGroup("ng1"), 1, now)
	ng1_4 := BuildTestNode("ng1-4", 1000, 1000)
	SetNodeReadyState(ng1_4, true, now.Add(-1*time.Minute))
	provider.AddNode("ng1", ng1_4)
	err = clusterstate.UpdateNodes([]*apiv1.Node{ng1_1, ng1_2, ng1_3, ng1_4}, nil, now)
	assert.NoError(t, err)
	assert.True(t, clusterstate.IsClusterHealthy())
	assert.True(t, clusterstate.IsNodeGroupHealthy("ng1"))
	assert.True(t, clusterstate.IsNodeGroupSafeToScaleUp(ng1, now))
	assert.False(t, clusterstate.backoff.IsBackedOff(ng1, nil, now))
}

func TestGetClusterSize(t *testing.T) {
	now := time.Now()

	ng1_1 := BuildTestNode("ng1-1", 1000, 1000)
	SetNodeReadyState(ng1_1, true, now.Add(-time.Minute))
	ng2_1 := BuildTestNode("ng2-1", 1000, 1000)
	SetNodeReadyState(ng2_1, true, now.Add(-time.Minute))
	notAutoscaledNode := BuildTestNode("notAutoscaledNode", 1000, 1000)
	SetNodeReadyState(notAutoscaledNode, true, now.Add(-time.Minute))

	provider := testprovider.NewTestCloudProvider(nil, nil)
	provider.AddNodeGroup("ng1", 1, 10, 5)
	provider.AddNodeGroup("ng2", 1, 10, 1)

	provider.AddNode("ng1", ng1_1)
	provider.AddNode("ng2", ng2_1)

	// Add a node not belonging to any autoscaled node group. This is to make sure that GetAutoscaledNodesCount doesn't
	// take nodes from non-autoscaled node groups into account.
	provider.AddNode("notAutoscaledNode", notAutoscaledNode)

	fakeClient := &fake.Clientset{}
	fakeLogRecorder, _ := utils.NewStatusMapRecorder(fakeClient, "kube-system", kube_record.NewFakeRecorder(5), false, "my-cool-configmap")
	clusterstate := NewClusterStateRegistry(provider, ClusterStateRegistryConfig{
		MaxTotalUnreadyPercentage: 10,
		OkTotalUnreadyCount:       1,
	}, fakeLogRecorder, newBackoff())

	// There are 2 actual nodes in 2 node groups with target sizes of 5 and 1.
	clusterstate.UpdateNodes([]*apiv1.Node{ng1_1, ng2_1, notAutoscaledNode}, nil, now)
	currentSize, targetSize := clusterstate.GetAutoscaledNodesCount()
	assert.Equal(t, 2, currentSize)
	assert.Equal(t, 6, targetSize)

	// Current size should increase after a new node is added.
	clusterstate.UpdateNodes([]*apiv1.Node{ng1_1, ng1_1, notAutoscaledNode, ng2_1}, nil, now.Add(time.Minute))
	currentSize, targetSize = clusterstate.GetAutoscaledNodesCount()
	assert.Equal(t, 3, currentSize)
	assert.Equal(t, 6, targetSize)

	// Target size should increase after a new node group is added.
	provider.AddNodeGroup("ng3", 1, 10, 1)
	clusterstate.UpdateNodes([]*apiv1.Node{ng1_1, ng1_1, notAutoscaledNode, ng2_1}, nil, now.Add(2*time.Minute))
	currentSize, targetSize = clusterstate.GetAutoscaledNodesCount()
	assert.Equal(t, 3, currentSize)
	assert.Equal(t, 7, targetSize)

	// Target size should change after a node group changes its target size.
	for _, ng := range provider.NodeGroups() {
		ng.(*testprovider.TestNodeGroup).SetTargetSize(10)
	}
	clusterstate.UpdateNodes([]*apiv1.Node{ng1_1, ng1_1, notAutoscaledNode, ng2_1}, nil, now.Add(3*time.Minute))
	currentSize, targetSize = clusterstate.GetAutoscaledNodesCount()
	assert.Equal(t, 3, currentSize)
	assert.Equal(t, 30, targetSize)
}

func TestUpdateScaleUp(t *testing.T) {
	now := time.Now()
	later := now.Add(time.Minute)

	provider := testprovider.NewTestCloudProvider(nil, nil)
	provider.AddNodeGroup("ng1", 1, 10, 5)
	fakeClient := &fake.Clientset{}
	fakeLogRecorder, _ := utils.NewStatusMapRecorder(fakeClient, "kube-system", kube_record.NewFakeRecorder(5), false, "my-cool-configmap")
	clusterstate := NewClusterStateRegistry(
		provider,
		ClusterStateRegistryConfig{
			MaxTotalUnreadyPercentage: 10,
			OkTotalUnreadyCount:       1,
			MaxNodeProvisionTime:      10 * time.Second,
		},
		fakeLogRecorder,
		newBackoff())

	clusterstate.RegisterOrUpdateScaleUp(provider.GetNodeGroup("ng1"), 100, now)
	assert.Equal(t, clusterstate.scaleUpRequests["ng1"].Increase, 100)
	assert.Equal(t, clusterstate.scaleUpRequests["ng1"].Time, now)
	assert.Equal(t, clusterstate.scaleUpRequests["ng1"].ExpectedAddTime, now.Add(10*time.Second))

	// expect no change of times on negative delta
	clusterstate.RegisterOrUpdateScaleUp(provider.GetNodeGroup("ng1"), -20, later)
	assert.Equal(t, clusterstate.scaleUpRequests["ng1"].Increase, 80)
	assert.Equal(t, clusterstate.scaleUpRequests["ng1"].Time, now)
	assert.Equal(t, clusterstate.scaleUpRequests["ng1"].ExpectedAddTime, now.Add(10*time.Second))

	// update times on positive delta
	clusterstate.RegisterOrUpdateScaleUp(provider.GetNodeGroup("ng1"), 30, later)
	assert.Equal(t, clusterstate.scaleUpRequests["ng1"].Increase, 110)
	assert.Equal(t, clusterstate.scaleUpRequests["ng1"].Time, later)
	assert.Equal(t, clusterstate.scaleUpRequests["ng1"].ExpectedAddTime, later.Add(10*time.Second))

	// if we get below 0 scalup is deleted
	clusterstate.RegisterOrUpdateScaleUp(provider.GetNodeGroup("ng1"), -200, now)
	assert.Nil(t, clusterstate.scaleUpRequests["ng1"])

	// If new scalup is registered with negative delta nothing should happen
	clusterstate.RegisterOrUpdateScaleUp(provider.GetNodeGroup("ng1"), -200, now)
	assert.Nil(t, clusterstate.scaleUpRequests["ng1"])
}

func TestScaleUpFailures(t *testing.T) {
	now := time.Now()

	provider := testprovider.NewTestCloudProvider(nil, nil)
	provider.AddNodeGroup("ng1", 0, 10, 0)
	provider.AddNodeGroup("ng2", 0, 10, 0)
	assert.NotNil(t, provider)

	fakeClient := &fake.Clientset{}
	fakeLogRecorder, _ := utils.NewStatusMapRecorder(fakeClient, "kube-system", kube_record.NewFakeRecorder(5), false, "my-cool-configmap")
	clusterstate := NewClusterStateRegistry(provider, ClusterStateRegistryConfig{}, fakeLogRecorder, newBackoff())

	clusterstate.RegisterFailedScaleUp(provider.GetNodeGroup("ng1"), metrics.Timeout, now)
	clusterstate.RegisterFailedScaleUp(provider.GetNodeGroup("ng2"), metrics.Timeout, now)
	clusterstate.RegisterFailedScaleUp(provider.GetNodeGroup("ng1"), metrics.APIError, now.Add(time.Minute))

	failures := clusterstate.GetScaleUpFailures()
	assert.Equal(t, map[string][]ScaleUpFailure{
		"ng1": {
			{NodeGroup: provider.GetNodeGroup("ng1"), Reason: metrics.Timeout, Time: now},
			{NodeGroup: provider.GetNodeGroup("ng1"), Reason: metrics.APIError, Time: now.Add(time.Minute)},
		},
		"ng2": {
			{NodeGroup: provider.GetNodeGroup("ng2"), Reason: metrics.Timeout, Time: now},
		},
	}, failures)

	clusterstate.clearScaleUpFailures()
	assert.Empty(t, clusterstate.GetScaleUpFailures())
}

func newBackoff() backoff.Backoff {
	return backoff.NewIdBasedExponentialBackoff(5*time.Minute, /*InitialNodeGroupBackoffDuration*/
		30*time.Minute /*MaxNodeGroupBackoffDuration*/, 3*time.Hour /*NodeGroupBackoffResetTimeout*/)
}
