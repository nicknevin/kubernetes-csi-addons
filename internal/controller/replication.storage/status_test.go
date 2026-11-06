/*
Copyright 2026 The Kubernetes-CSI-Addons Authors.

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

package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/csi-addons/kubernetes-csi-addons/api/replication.storage/v1alpha1"
)

func TestIsDestinationInfoAvailable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		conditions []metav1.Condition
		want       bool
	}{
		{
			name:       "no conditions",
			conditions: nil,
			want:       false,
		},
		{
			name: "condition absent",
			conditions: []metav1.Condition{
				{
					Type:   v1alpha1.ConditionCompleted,
					Status: metav1.ConditionTrue,
				},
			},
			want: false,
		},
		{
			name: "condition present and true",
			conditions: []metav1.Condition{
				{
					Type:   v1alpha1.ConditionDestinationInfoAvailable,
					Status: metav1.ConditionTrue,
				},
			},
			want: true,
		},
		{
			name: "condition present but false",
			conditions: []metav1.Condition{
				{
					Type:   v1alpha1.ConditionDestinationInfoAvailable,
					Status: metav1.ConditionFalse,
				},
			},
			want: false,
		},
		{
			name: "condition present but unknown",
			conditions: []metav1.Condition{
				{
					Type:   v1alpha1.ConditionDestinationInfoAvailable,
					Status: metav1.ConditionUnknown,
				},
			},
			want: false,
		},
	}
	for _, tt := range tests {
		newtt := tt
		t.Run(newtt.name, func(t *testing.T) {
			t.Parallel()
			got := isDestinationInfoAvailable(newtt.conditions)
			assert.Equal(t, newtt.want, got)
		})
	}
}

func TestSetDestinationInfoAvailableCondition(t *testing.T) {
	t.Parallel()
	conditions := []metav1.Condition{}
	setDestinationInfoAvailableCondition(&conditions, 1, pvcDataSource)

	assert.Len(t, conditions, 1)
	assert.Equal(t, v1alpha1.ConditionDestinationInfoAvailable, conditions[0].Type)
	assert.Equal(t, metav1.ConditionTrue, conditions[0].Status)
	assert.Equal(t, v1alpha1.DestinationInfoUpdated, conditions[0].Reason)
	assert.Contains(t, conditions[0].Message, string(v1alpha1.Volume))
	assert.Contains(t, conditions[0].Message, v1alpha1.MessageDestinationInfoAvailable)
	assert.Equal(t, int64(1), conditions[0].ObservedGeneration)
}

func TestSetDestinationInfoPendingCondition(t *testing.T) {
	t.Parallel()
	conditions := []metav1.Condition{}
	setDestinationInfoPendingCondition(&conditions, 2, volumeGroupReplicationDataSource)

	assert.Len(t, conditions, 1)
	assert.Equal(t, v1alpha1.ConditionDestinationInfoAvailable, conditions[0].Type)
	assert.Equal(t, metav1.ConditionFalse, conditions[0].Status)
	assert.Equal(t, v1alpha1.DestinationInfoPending, conditions[0].Reason)
	assert.Contains(t, conditions[0].Message, string(v1alpha1.VolumeGroup))
	assert.Contains(t, conditions[0].Message, v1alpha1.MessageDestinationInfoPending)
	assert.Equal(t, int64(2), conditions[0].ObservedGeneration)
}

func TestSetDestinationInfoFailedCondition(t *testing.T) {
	t.Parallel()
	conditions := []metav1.Condition{}
	setDestinationInfoFailedCondition(&conditions, 3, pvcDataSource, "rpc unavailable")

	assert.Len(t, conditions, 1)
	assert.Equal(t, v1alpha1.ConditionDestinationInfoAvailable, conditions[0].Type)
	assert.Equal(t, metav1.ConditionFalse, conditions[0].Status)
	assert.Equal(t, v1alpha1.FailedToGetDestinationInfo, conditions[0].Reason)
	assert.Contains(t, conditions[0].Message, v1alpha1.MessageDestinationInfoFailed)
	assert.Contains(t, conditions[0].Message, "rpc unavailable")
	assert.Equal(t, int64(3), conditions[0].ObservedGeneration)
}

func TestDestinationInfoConditionTransitions(t *testing.T) {
	t.Parallel()

	// Start with pending
	conditions := []metav1.Condition{}
	setDestinationInfoPendingCondition(&conditions, 1, pvcDataSource)
	assert.False(t, isDestinationInfoAvailable(conditions))

	// Transition to failed
	setDestinationInfoFailedCondition(&conditions, 1, pvcDataSource, "not ready")
	assert.False(t, isDestinationInfoAvailable(conditions))
	assert.Len(t, conditions, 1) // should update in place, not add

	// Transition to available
	setDestinationInfoAvailableCondition(&conditions, 2, pvcDataSource)
	assert.True(t, isDestinationInfoAvailable(conditions))
	assert.Len(t, conditions, 1) // still one condition, updated in place

	// Transition back to pending (e.g., membership change)
	setDestinationInfoPendingCondition(&conditions, 3, pvcDataSource)
	assert.False(t, isDestinationInfoAvailable(conditions))
	assert.Len(t, conditions, 1)
}

func TestRefreshValidatedCondition(t *testing.T) {
	t.Parallel()

	staleTrue := metav1.Condition{
		Type: v1alpha1.ConditionValidated, Status: metav1.ConditionTrue,
		Reason: v1alpha1.PrerequisiteMet, ObservedGeneration: 1,
	}
	staleFalse := metav1.Condition{
		Type: v1alpha1.ConditionValidated, Status: metav1.ConditionFalse,
		Reason: v1alpha1.PrerequisiteNotMet, ObservedGeneration: 1,
	}

	tests := []struct {
		name       string
		spec       v1alpha1.ReplicationState
		state      v1alpha1.State
		existing   []metav1.Condition
		wantStatus metav1.ConditionStatus
		wantReason string
		wantGen    int64
	}{
		{
			// dr2 log: gen 1 promoted, gen 2 resync failing on every retry.
			name: "resync after primary refreshes stale True", spec: v1alpha1.Resync, state: v1alpha1.PrimaryState,
			existing: []metav1.Condition{staleTrue}, wantStatus: metav1.ConditionTrue, wantReason: v1alpha1.PrerequisiteMet, wantGen: 5,
		},
		{
			name: "secondary refreshes stale False", spec: v1alpha1.Secondary, state: v1alpha1.PrimaryState,
			existing: []metav1.Condition{staleFalse}, wantStatus: metav1.ConditionTrue, wantReason: v1alpha1.PrerequisiteMet, wantGen: 5,
		},
		{
			name: "already primary refreshes stale False", spec: v1alpha1.Primary, state: v1alpha1.PrimaryState,
			existing: []metav1.Condition{staleFalse}, wantStatus: metav1.ConditionTrue, wantReason: v1alpha1.PrerequisiteMet, wantGen: 5,
		},
		{
			name: "pending enable refreshes stale True", spec: v1alpha1.Primary, state: v1alpha1.SecondaryState,
			existing: []metav1.Condition{staleTrue}, wantStatus: metav1.ConditionTrue, wantReason: v1alpha1.PrerequisiteMet, wantGen: 5,
		},
		{
			name: "pending enable creates missing condition", spec: v1alpha1.Primary, state: "",
			wantStatus: metav1.ConditionTrue, wantReason: v1alpha1.PrerequisiteMet, wantGen: 5,
		},
		{
			// enableReplication will decide; don't flip False->True->False.
			name: "pending enable leaves False alone", spec: v1alpha1.Primary, state: v1alpha1.SecondaryState,
			existing: []metav1.Condition{staleFalse}, wantStatus: metav1.ConditionFalse, wantReason: v1alpha1.PrerequisiteNotMet, wantGen: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			vr := &v1alpha1.VolumeReplication{}
			vr.Generation = 5
			vr.Spec.ReplicationState = tt.spec
			vr.Spec.DataSource.Kind = pvcDataSource
			vr.Status.State = tt.state
			vr.Status.Conditions = append([]metav1.Condition{}, tt.existing...)

			refreshValidatedCondition(vr)

			c := findCondition(vr.Status.Conditions, v1alpha1.ConditionValidated)
			if assert.NotNil(t, c) {
				assert.Equal(t, tt.wantStatus, c.Status)
				assert.Equal(t, tt.wantReason, c.Reason)
				assert.Equal(t, tt.wantGen, c.ObservedGeneration)
			}
		})
	}
}
