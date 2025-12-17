/*
Copyright The Kubernetes NMState Authors.


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

package metrics

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	"github.com/pkg/errors"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	nmstatev1beta1 "github.com/nmstate/kubernetes-nmstate/api/v1beta1"
	"github.com/nmstate/kubernetes-nmstate/pkg/monitoring"
	"github.com/nmstate/kubernetes-nmstate/pkg/state"
)

// NodeNetworkStateReconciler reconciles a NodeNetworkState object for metrics
type NodeNetworkStateReconciler struct {
	client.Client
	Log     logr.Logger
	Scheme  *runtime.Scheme
	oldNNSs map[string]map[string]int // map of NNS name to interface type counts
}

// Reconcile reads the state of the cluster for a NodeNetworkState object and calculates
// metrics for network interface counts by type.
func (r *NodeNetworkStateReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("metrics.nodenetworkstate", request.NamespacedName)
	log.Info("Reconcile")

	nnsInstance := &nmstatev1beta1.NodeNetworkState{}
	err := r.Client.Get(ctx, request.NamespacedName, nnsInstance)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// NNS has been deleted, clean up the old NNS map
			delete(r.oldNNSs, request.Name)
			return ctrl.Result{}, nil
		}
		log.Error(err, "Error retrieving NodeNetworkState")
		return ctrl.Result{}, err
	}

	if err := r.reportStatistics(ctx); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed reporting statistics: %w", err)
	}

	// Store current interface counts for future delta calculation
	counts, err := state.CountInterfacesByType(nnsInstance.Status.CurrentState)
	if err != nil {
		log.Error(err, "Failed to count interfaces by type")
	} else {
		r.oldNNSs[nnsInstance.Name] = counts
	}

	return ctrl.Result{}, nil
}

func (r *NodeNetworkStateReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.oldNNSs = map[string]map[string]int{}

	onCreationOrUpdateForThisNNS := predicate.Funcs{
		CreateFunc: func(createEvent event.CreateEvent) bool {
			return true
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return true
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldNNS, ok := e.ObjectOld.(*nmstatev1beta1.NodeNetworkState)
			if !ok {
				return false
			}
			newNNS, ok := e.ObjectNew.(*nmstatev1beta1.NodeNetworkState)
			if !ok {
				return false
			}

			// Reconcile if the current state has changed
			return oldNNS.Status.CurrentState.String() != newNNS.Status.CurrentState.String()
		},
		GenericFunc: func(event.GenericEvent) bool {
			return false
		},
	}

	err := ctrl.NewControllerManagedBy(mgr).
		For(&nmstatev1beta1.NodeNetworkState{}).
		WithEventFilter(onCreationOrUpdateForThisNNS).
		Complete(r)
	if err != nil {
		return errors.Wrap(err, "failed to add controller to NNS metrics Reconciler")
	}

	return nil
}

func (r *NodeNetworkStateReconciler) reportStatistics(ctx context.Context) error {
	nnsList := nmstatev1beta1.NodeNetworkStateList{}
	if err := r.List(ctx, &nnsList); err != nil {
		return err
	}

	// Calculate old and new cluster-wide interface type counts
	oldCounts := make(map[string]int)
	newCounts := make(map[string]int)

	for i := range nnsList.Items {
		nns := &nnsList.Items[i]

		// Get new counts from current state
		counts, err := state.CountInterfacesByType(nns.Status.CurrentState)
		if err != nil {
			r.Log.Error(err, "Failed to count interfaces by type", "nns", nns.Name)
			continue
		}
		for ifaceType, count := range counts {
			newCounts[ifaceType] += count
		}

		// Get old counts if available
		if oldNNSCounts, ok := r.oldNNSs[nns.Name]; ok {
			for ifaceType, count := range oldNNSCounts {
				oldCounts[ifaceType] += count
			}
		}
	}

	// Calculate delta and update metrics
	// First, handle types that exist in new counts
	for ifaceType, newCount := range newCounts {
		oldCount := oldCounts[ifaceType]
		delta := newCount - oldCount
		if delta > 0 {
			monitoring.NetworkInterfaces.WithLabelValues(ifaceType).Add(float64(delta))
		} else if delta < 0 {
			monitoring.NetworkInterfaces.WithLabelValues(ifaceType).Sub(float64(-delta))
		}
	}

	// Handle types that exist in old counts but not in new counts (removed interfaces)
	for ifaceType, oldCount := range oldCounts {
		if _, exists := newCounts[ifaceType]; !exists {
			monitoring.NetworkInterfaces.WithLabelValues(ifaceType).Sub(float64(oldCount))
		}
	}

	return nil
}
