package util

import (
	"fmt"
	"math"
	"reflect"
	"sort"
	"time"

	vpa_types "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/apis/autoscaling.k8s.io/v1"
)

//  1. If the histogram is new: never filled with minAllowedContainerCnt+ container metrics,
//     we will always override percentile value as max allowed value
//  2. Last x containers are allowed to override percentile value with their percentiled max usage values:
//     in case if the percentile value driven by too few containers / samples can be inaccurate.
type atleastxcontainerDecayingHistogram struct {
	decayingHistogram
	// whether the histogram has ever been filled with minAllowedContainerCnt+ container metrics
	isNewHistogram bool
	// Used to store last x container metrics
	containerIdToLatestTimestamp map[string]time.Time
	containerIdToMaxValue        map[string]float64
	minAllowedContainerCnt       int
	resourceForNewHistogram      float64
}

func NewAtleastxcontainerDecayingHistogram(minAllowedContainerCnt int, options HistogramOptions, halfLife time.Duration, resourceForNewHistogram float64) Histogram {
	return &atleastxcontainerDecayingHistogram{
		decayingHistogram: decayingHistogram{
			histogram:          *NewHistogram(options).(*histogram),
			halfLife:           halfLife,
			referenceTimestamp: time.Time{},
		},
		isNewHistogram:               true,
		containerIdToLatestTimestamp: make(map[string]time.Time),
		containerIdToMaxValue:        make(map[string]float64),
		minAllowedContainerCnt:       minAllowedContainerCnt,
		resourceForNewHistogram:      resourceForNewHistogram,
	}
}

func (h *atleastxcontainerDecayingHistogram) Percentile(percentile float64) float64 {
	h.updateAndShrinkMap()

	value := h.decayingHistogram.Percentile(percentile)
	if h.isNewHistogram {
		return math.Max(value, h.resourceForNewHistogram)
	}

	// if h.enableForOldHistogram {
	// 	return math.Max(h.decayingHistogram.Percentile(percentile), h.percentileOverLastXContainers(percentile))
	// }
	return value
}

func (h *atleastxcontainerDecayingHistogram) AddSample(containerId string, value float64, weight float64, time time.Time) {
	h.containerIdToLatestTimestamp[containerId] = time
	h.containerIdToMaxValue[containerId] = math.Max(h.containerIdToMaxValue[containerId], value)
	h.decayingHistogram.AddSample(containerId, value, weight, time)
}

func (h *atleastxcontainerDecayingHistogram) SubtractSample(containerId string, value float64, weight float64, time time.Time) {
	// No need to worry about potential sample substract in updateMinAndMaxBucket or in SubtractSample
	// h.containerIdToCnt is simply a rough estimation
	h.histogram.SubtractSample(containerId, value, weight, time)
}

func (h *atleastxcontainerDecayingHistogram) Merge(other Histogram) {
	o := other.(*atleastxcontainerDecayingHistogram)
	if o.minAllowedContainerCnt > h.minAllowedContainerCnt {
		h.minAllowedContainerCnt = o.minAllowedContainerCnt
	}
	for k, v := range o.containerIdToLatestTimestamp {
		value, exists := h.containerIdToLatestTimestamp[k]
		if !exists || value.Before(v) {
			h.containerIdToLatestTimestamp[k] = v
		}
	}
	for k, v := range o.containerIdToMaxValue {
		value, exists := h.containerIdToMaxValue[k]
		if !exists || value < v {
			h.containerIdToMaxValue[k] = v
		}
	}
	if !o.isNewHistogram {
		h.isNewHistogram = o.isNewHistogram
	}

	h.decayingHistogram.Merge(&o.decayingHistogram)
}

func (h *atleastxcontainerDecayingHistogram) Equals(other Histogram) bool {
	h2, typesMatch := (other).(*atleastxcontainerDecayingHistogram)
	return typesMatch && h.isNewHistogram == h2.isNewHistogram && reflect.DeepEqual(h.containerIdToLatestTimestamp, h2.containerIdToLatestTimestamp) && reflect.DeepEqual(h.containerIdToMaxValue, h2.containerIdToMaxValue) && reflect.DeepEqual(h.minAllowedContainerCnt, h2.minAllowedContainerCnt) && h.decayingHistogram.Equals(&h2.decayingHistogram)
}

func (h *atleastxcontainerDecayingHistogram) IsEmpty() bool {
	return h.decayingHistogram.IsEmpty()
}

func (h *atleastxcontainerDecayingHistogram) String() string {
	return fmt.Sprintf("isNewHistogram: %v, containerIdToLatestTimestamp: %v, containerIdToMaxValue: %v, minAllowedContainerCnt: %v\n%s", h.isNewHistogram, h.containerIdToLatestTimestamp, h.containerIdToMaxValue, h.minAllowedContainerCnt, h.decayingHistogram.String())
}

func (h *atleastxcontainerDecayingHistogram) SaveToChekpoint() (*vpa_types.HistogramCheckpoint, error) {
	checkpoint, err := h.decayingHistogram.SaveToChekpoint()
	if err != nil {
		return checkpoint, err
	}
	h.updateAndShrinkMap()

	checkpoint.IsNewHistogram = h.isNewHistogram
	checkpoint.ContainerIdToLatestTimestamp = h.containerIdToLatestTimestamp
	checkpoint.ContainerIdToMaxValue = h.containerIdToMaxValue
	return checkpoint, nil
}

func (h *atleastxcontainerDecayingHistogram) LoadFromCheckpoint(checkpoint *vpa_types.HistogramCheckpoint) error {
	err := h.decayingHistogram.LoadFromCheckpoint(checkpoint)
	if err != nil {
		return err
	}
	h.isNewHistogram = checkpoint.IsNewHistogram
	h.containerIdToLatestTimestamp = checkpoint.ContainerIdToLatestTimestamp
	h.containerIdToMaxValue = checkpoint.ContainerIdToMaxValue
	return nil
}

// Turn off isNewHistogram if >= minAllowedContainerCnt container metrics are collected
// and shrink containerIdToLatestTimestamp and containerIdToMaxValue map until only minAllowedContainerCnt items in the map
func (h *atleastxcontainerDecayingHistogram) updateAndShrinkMap() {
	if h.isNewHistogram {
		if len(h.containerIdToLatestTimestamp) >= h.minAllowedContainerCnt {
			h.isNewHistogram = false
		}
	}

	if len(h.containerIdToLatestTimestamp) > h.minAllowedContainerCnt {
		type kv struct {
			Key   string
			Value int64
		}

		var ss []kv
		for k, v := range h.containerIdToLatestTimestamp {
			ss = append(ss, kv{k, v.Unix()})
		}

		// From smallest to largest
		sort.Slice(ss, func(i, j int) bool {
			return ss[i].Value < ss[j].Value
		})

		for i := 0; i < len(ss)-h.minAllowedContainerCnt; i++ {
			delete(h.containerIdToLatestTimestamp, ss[i].Key)
			delete(h.containerIdToMaxValue, ss[i].Key)
		}
	}
}

func (h *atleastxcontainerDecayingHistogram) percentileOverLastXContainers(percentile float64) float64 {
	maxValueHistogram := NewHistogram(h.options)
	for _, v := range h.containerIdToMaxValue {
		maxValueHistogram.AddSample("", v, 1.0, time.Now())
	}
	return maxValueHistogram.Percentile(percentile)
}
