/*
Copyright 2018 The Kubernetes Authors.

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

package model

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	labels "k8s.io/apimachinery/pkg/labels"
	vpa_types "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/apis/autoscaling.k8s.io/v1"
)

var (
	testPodID1       = PodID{"namespace-1", "pod-1"}
	testPodID2       = PodID{"namespace-1", "pod-2"}
	testContainerID1 = ContainerID{testPodID1, "container-1"}
	testRequest      = Resources{
		ResourceCPU:    CPUAmountFromCores(3.14),
		ResourceMemory: MemoryAmountFromBytes(3.14e9),
	}
	testTimestamp              = time.Now()
	testMinAllowedContainerCnt = 0
)

func addTestCPUSample(cluster *ClusterState, container ContainerID, cpuCores float64) error {
	sample := ContainerUsageSampleWithKey{
		Container: container,
		ContainerUsageSample: ContainerUsageSample{
			MeasureStart: testTimestamp,
			Usage:        CPUAmountFromCores(cpuCores),
			Request:      testRequest[ResourceCPU],
			Resource:     ResourceCPU,
		},
	}
	return cluster.AddSample(&sample)
}

func addTestMemorySample(cluster *ClusterState, container ContainerID, memoryBytes float64) error {
	sample := ContainerUsageSampleWithKey{
		Container: container,
		ContainerUsageSample: ContainerUsageSample{
			MeasureStart: testTimestamp,
			Usage:        MemoryAmountFromBytes(memoryBytes),
			Request:      testRequest[ResourceMemory],
			Resource:     ResourceMemory,
		},
	}
	return cluster.AddSample(&sample)
}

// Creates two pods, each having two containers:
//
//	testPodID1: { 'app-A', 'app-B' }
//	testPodID2: { 'app-A', 'app-C' }
//
// Adds a few usage samples to the containers.
// Verifies that AggregateStateByContainerName() properly aggregates
// container CPU and memory peak histograms, grouping the two containers
// with the same name ('app-A') together.
func TestAggregateStateByContainerName(t *testing.T) {
	cluster := NewClusterState()
	cluster.AddOrUpdatePod(testPodID1, testLabels, apiv1.PodRunning)
	otherLabels := labels.Set{"label-2": "value-2"}
	cluster.AddOrUpdatePod(testPodID2, otherLabels, apiv1.PodRunning)

	// Create 4 containers: 2 with the same name and 2 with different names.
	containers := []ContainerID{
		{testPodID1, "app-A"},
		{testPodID1, "app-B"},
		{testPodID2, "app-A"},
		{testPodID2, "app-C"},
	}
	for _, c := range containers {
		assert.NoError(t, cluster.AddOrUpdateContainer(c, testRequest))
	}

	// Add CPU usage samples to all containers.
	assert.NoError(t, addTestCPUSample(cluster, containers[0], 1.0)) // app-A
	assert.NoError(t, addTestCPUSample(cluster, containers[1], 5.0)) // app-B
	assert.NoError(t, addTestCPUSample(cluster, containers[2], 3.0)) // app-A
	assert.NoError(t, addTestCPUSample(cluster, containers[3], 5.0)) // app-C
	// Add Memory usage samples to all containers.
	assert.NoError(t, addTestMemorySample(cluster, containers[0], 2e9))  // app-A
	assert.NoError(t, addTestMemorySample(cluster, containers[1], 10e9)) // app-B
	assert.NoError(t, addTestMemorySample(cluster, containers[2], 4e9))  // app-A
	assert.NoError(t, addTestMemorySample(cluster, containers[3], 10e9)) // app-C

	// Build the AggregateContainerStateMap.
	aggregateResources := AggregateStateByContainerName(cluster.aggregateStateMap)
	assert.Contains(t, aggregateResources, "app-A")
	assert.Contains(t, aggregateResources, "app-B")
	assert.Contains(t, aggregateResources, "app-C")

	// Expect samples from all containers to be grouped by the container name.
	assert.Equal(t, 2, aggregateResources["app-A"].TotalSamplesCount)
	assert.Equal(t, 1, aggregateResources["app-B"].TotalSamplesCount)
	assert.Equal(t, 1, aggregateResources["app-C"].TotalSamplesCount)
}

func TestAggregateContainerStateSaveToCheckpoint(t *testing.T) {
	location, _ := time.LoadLocation("UTC")
	cs := NewAggregateContainerState()
	t1, t2 := time.Date(2018, time.January, 1, 2, 3, 4, 0, location), time.Date(2018, time.February, 1, 2, 3, 4, 0, location)
	cs.FirstSampleStart = t1
	cs.LastSampleStart = t2
	cs.TotalSamplesCount = 10

	cs.AggregateCPUUsage.AddSample("", 1, 33, t2)
	cs.AggregateMemoryPeaks.AddSample("", 1, 55, t1)
	cs.AggregateMemoryPeaks.AddSample("", 10000000, 55, t1)
	checkpoint, err := cs.SaveToCheckpoint()

	assert.NoError(t, err)

	assert.Equal(t, t1, checkpoint.FirstSampleStart.Time)
	assert.Equal(t, t2, checkpoint.LastSampleStart.Time)
	assert.Equal(t, 10, checkpoint.TotalSamplesCount)

	assert.Equal(t, SupportedCheckpointVersion, checkpoint.Version)

	// Basic check that serialization of histograms happened.
	// Full tests are part of the Histogram.
	assert.Len(t, checkpoint.CPUHistogram.BucketWeights, 1)
	assert.Len(t, checkpoint.MemoryHistogram.BucketWeights, 2)
}

func TestAggregateContainerStateLoadFromCheckpointFailsForVersionMismatch(t *testing.T) {
	checkpoint := vpa_types.VerticalPodAutoscalerCheckpointStatus{
		Version: "foo",
	}
	cs := NewAggregateContainerState()
	err := cs.LoadFromCheckpoint(&checkpoint)
	assert.Error(t, err)
}

func TestAggregateContainerStateLoadFromCheckpoint(t *testing.T) {
	location, _ := time.LoadLocation("UTC")
	t1, t2 := time.Date(2018, time.January, 1, 2, 3, 4, 0, location), time.Date(2018, time.February, 1, 2, 3, 4, 0, location)

	checkpoint := vpa_types.VerticalPodAutoscalerCheckpointStatus{
		Version:           SupportedCheckpointVersion,
		FirstSampleStart:  metav1.NewTime(t1),
		LastSampleStart:   metav1.NewTime(t2),
		TotalSamplesCount: 20,
		MemoryHistogram: vpa_types.HistogramCheckpoint{
			BucketWeights: map[int]float64{
				10: 0,
				11: 0,
				12: 1.963459636189532,
				13: 0,
				14: 0.16277002330302742,
				15: 4.727735819254221,
				16: 0,
				17: 0.5907843371701504,
				18: 0.18191943780926595,
				19: 0.440436533643486,
				20: 0.03829882901247704,
				21: 3.8661266218194195,
				22: 0.16277002330302742,
				23: 0.014362060879678892,
				24: 3.190165778507044,
				25: 0.21543091319518337,
				26: 4.0618867570926,
				27: 6.629469245188258,
				28: 4.8165399242628215,
				29: 4.455549987410929,
				3:  7.081832061517862,
				30: 2.224011592925153,
				31: 10.824726974852656,
				32: 0.6175686178261923,
				33: 0.34468946111229337,
				34: 7.461656420305497,
				35: 7.738685475482372,
				36: 24.97323735626143,
				37: 9.55116646703675,
				38: 10.857409272395568,
				39: 8.285692518582582,
				4:  9.539180114807648,
				40: 9.779883066304304,
				41: 15.44040804901226,
				42: 10.84025753919446,
				43: 4.047554236515806,
				44: 6.217255766752439,
				45: 6.3624333334198875,
				46: 10.393036399654108,
				47: 6.959365499672178,
				48: 14.654250236282222,
				49: 8.416526057816524,
				5:  1.4065991674590717,
				50: 8.690458405929231,
				51: 20.99946591194034,
				52: 51.21184969616761,
				53: 73.01745386897426,
				54: 63.407600572968825,
				55: 6.838026941483867,
				56: 19.151778550408224,
				57: 97.3169310504436,
				58: 208.596902476028,
				59: 246.62711352345977,
				6:  0.0622355971452752,
				60: 145.9517976119986,
				61: 63.97841491284263,
				62: 9.648333529298363,
				63: 0.014362060879678892,
				64: 0.4978847771622016,
				7:  17.19677339034477,
				8:  5.07843352522292,
				9:  0,
			},
			TotalWeight:    1265.9423715222538,
			IsNewHistogram: false,
		},
		CPUHistogram: vpa_types.HistogramCheckpoint{
			BucketWeights: map[int]float64{
				0:  8.419546308410279,
				1:  0.0007858870879793461,
				10: 0.0002459284555892854,
				11: 0.00011796895308087605,
				12: 0.0003355621276560582,
				13: 0.000216991517754821,
				14: 0.0005743127767282701,
				15: 0.000318866570123749,
				16: 0.00015995930924708507,
				17: 6.852092266555223e-05,
				18: 0.0003206700492991974,
				19: 0.0002352291189403414,
				2:  0.0005507016508207401,
				20: 0.00040776426734928574,
				21: 0.00011232203310363134,
				22: 0.12397930596364765,
				23: 0.00025492049668576875,
				24: 0.0004519434943781542,
				25: 0.0007245938614079959,
				26: 0.5342010735474688,
				27: 0.5348588736813712,
				28: 0.0009406750516075084,
				29: 1.3510505970552142,
				3:  0.12256622288659862,
				30: 0.12288050234931017,
				31: 0.00024412249098276225,
				32: 0.12508878771767065,
				33: 0.00036272306035060273,
				34: 0.004162051377172809,
				35: 0.0004490194983028447,
				36: 0.0074845866098971914,
				37: 0.0001733872127586003,
				38: 0.18678479106104456,
				39: 0.3682870491475493,
				4:  0.00018159086454621625,
				40: 0.1222917953207694,
				5:  0.0004761286177864503,
				6:  0.12419307676840297,
				7:  0.00013069228637203732,
				8:  0.12231728859386955,
				9:  0.0002474258556938906,
			},
			TotalWeight:    12.278780218121478,
			IsNewHistogram: false,
		},
	}

	cs := NewAggregateContainerState()
	err := cs.LoadFromCheckpoint(&checkpoint)

	fmt.Printf("Pod not present in the ClusterState: %v", cs.AggregateCPUUsage)

	assert.NoError(t, err)

	println(cs.AggregateMemoryPeaks.Percentile(1.0))
	println(cs.AggregateCPUUsage.Percentile(1.0))
}

func TestAggregateContainerStateIsExpired(t *testing.T) {
	cs := NewAggregateContainerState()
	cs.LastSampleStart = testTimestamp
	cs.TotalSamplesCount = 1
	assert.False(t, cs.isExpired(testTimestamp.Add(7*24*time.Hour)))
	assert.True(t, cs.isExpired(testTimestamp.Add(8*24*time.Hour)))

	csEmpty := NewAggregateContainerState()
	csEmpty.TotalSamplesCount = 0
	csEmpty.CreationTime = testTimestamp
	assert.False(t, csEmpty.isExpired(testTimestamp.Add(7*24*time.Hour)))
	assert.True(t, csEmpty.isExpired(testTimestamp.Add(8*24*time.Hour)))
}

func TestUpdateFromPolicyScalingMode(t *testing.T) {
	scalingModeAuto := vpa_types.ContainerScalingModeAuto
	scalingModeOff := vpa_types.ContainerScalingModeOff
	testCases := []struct {
		name     string
		policy   *vpa_types.ContainerResourcePolicy
		expected *vpa_types.ContainerScalingMode
	}{
		{
			name: "Explicit auto scaling mode",
			policy: &vpa_types.ContainerResourcePolicy{
				Mode: &scalingModeAuto,
			},
			expected: &scalingModeAuto,
		}, {
			name: "Off scaling mode",
			policy: &vpa_types.ContainerResourcePolicy{
				Mode: &scalingModeOff,
			},
			expected: &scalingModeOff,
		}, {
			name:     "No mode specified - default to Auto",
			policy:   &vpa_types.ContainerResourcePolicy{},
			expected: &scalingModeAuto,
		}, {
			name:     "Nil policy - default to Auto",
			policy:   nil,
			expected: &scalingModeAuto,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cs := NewAggregateContainerState()
			cs.UpdateFromPolicy(tc.policy)
			assert.Equal(t, tc.expected, cs.GetScalingMode())
		})
	}
}

func TestUpdateFromPolicyControlledResources(t *testing.T) {
	testCases := []struct {
		name     string
		policy   *vpa_types.ContainerResourcePolicy
		expected []ResourceName
	}{
		{
			name: "Explicit ControlledResources",
			policy: &vpa_types.ContainerResourcePolicy{
				ControlledResources: &[]apiv1.ResourceName{apiv1.ResourceCPU, apiv1.ResourceMemory},
			},
			expected: []ResourceName{ResourceCPU, ResourceMemory},
		}, {
			name: "Empty ControlledResources",
			policy: &vpa_types.ContainerResourcePolicy{
				ControlledResources: &[]apiv1.ResourceName{},
			},
			expected: []ResourceName{},
		}, {
			name: "ControlledResources with one resource",
			policy: &vpa_types.ContainerResourcePolicy{
				ControlledResources: &[]apiv1.ResourceName{apiv1.ResourceMemory},
			},
			expected: []ResourceName{ResourceMemory},
		}, {
			name:     "No ControlledResources specified - used default",
			policy:   &vpa_types.ContainerResourcePolicy{},
			expected: []ResourceName{ResourceCPU, ResourceMemory},
		}, {
			name:     "Nil policy - use default",
			policy:   nil,
			expected: []ResourceName{ResourceCPU, ResourceMemory},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cs := NewAggregateContainerState()
			cs.UpdateFromPolicy(tc.policy)
			assert.Equal(t, tc.expected, cs.GetControlledResources())
		})
	}
}
