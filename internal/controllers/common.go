/*
Copyright 2026 The cluster-api-ipam-provider-netbox Authors.

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

// Package controllers reconciles NetBoxNetwork, NetBoxIPPool and IPAddressClaim.
package controllers

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	ipamv1 "sigs.k8s.io/cluster-api/api/ipam/v1beta2"
	clusterctlv1 "sigs.k8s.io/cluster-api/cmd/clusterctl/api/v1alpha3"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/api/v1alpha1"
	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/internal/netbox"
)

const clusterNameField = "spec.clusterName"

// asType casts o to T; any other type is logged and reported as !ok.
func asType[T client.Object](ctx context.Context, o client.Object) (T, bool) {
	t, ok := o.(T)
	if !ok {
		ctrl.LoggerFrom(ctx).Error(nil, "unexpected object type", "want", fmt.Sprintf("%T", t), "got", fmt.Sprintf("%T", o))
	}
	return t, ok
}

// clusterEvents passes the Cluster changes that matter: creation, spec (pause), annotations (pause).
var clusterEvents = predicate.Or(
	predicate.GenerationChangedPredicate{},
	predicate.AnnotationChangedPredicate{},
)

// netBoxWrites serialises the NetBox sections that check, then create or change address space: NetBox checks
// range overlap and IP and prefix uniqueness in clean(), without a lock, so two parallel writes can both pass.
// Held only while a section writes, taken once per operation, never nested; reads run first without it (underLock).
// ponytail: one process (leader election); management planes with other owners that share a parent are not covered.
// Upgrade: per-parent locks if it throttles, a lock or constraint in NetBox for many planes.
var netBoxWrites sync.Mutex

// netBoxWriteTimeout bounds a section under netBoxWrites, so a slow NetBox cannot hold every reconcile for long.
const netBoxWriteTimeout = 30 * time.Second

// underLock runs op on a read-only NetBox client without the lock; only when op has something to write
// it runs again on nb under netBoxWrites. Steady-state reconciles never wait for the lock.
// op reads everything anew on each run and leaves no trace of a run that stopped at ErrWriteNeeded.
// A result that depends on a read racing with a write of ours is not final on the first pass: op returns
// ErrWriteNeeded for it, so that the locked pass decides.
func underLock(ctx context.Context, nb *netbox.Client, op func(context.Context, *netbox.Client) error) error {
	if err := op(ctx, nb.ReadOnly()); !errors.Is(err, netbox.ErrWriteNeeded) {
		return err
	}
	return locked(ctx, nb, op)
}

// locked runs op on nb under netBoxWrites, within netBoxWriteTimeout.
func locked(ctx context.Context, nb *netbox.Client, op func(context.Context, *netbox.Client) error) error {
	start := time.Now()
	netBoxWrites.Lock()
	defer netBoxWrites.Unlock()
	lockWait.Observe(time.Since(start).Seconds())
	ctx, cancel := context.WithTimeout(ctx, netBoxWriteTimeout)
	defer cancel()
	return op(ctx, nb)
}

// normalEvent records a change the provider made in NetBox as a Normal Event on obj.
func normalEvent(rec events.EventRecorder, obj runtime.Object, reason, note string, args ...any) {
	rec.Eventf(obj, nil, corev1.EventTypeNormal, reason, "Reconcile", note, args...)
}

// refOf is the key of a Kubernetes object in NetBox. Unlike the UID it survives clusterctl move.
func refOf(kind string, o client.Object) string {
	return kind + "/" + o.GetNamespace() + "/" + o.GetName()
}

// netBoxRequeueAfter is how often a NetBox state that only an outside fix can change is checked again.
const netBoxRequeueAfter = time.Minute

type errKind int

const (
	kindError errKind = iota // unexpected: back to controller-runtime, which backs off
	kindWait                 // a watch on the thing waited for wakes the object
	kindRetry                // NetBox state: checked again after netBoxRequeueAfter
)

// reasonError carries the reason the Ready condition reports for an error and how to requeue.
type reasonError struct {
	reason string
	kind   errKind
	err    error
}

func (e reasonError) Error() string { return e.err.Error() }
func (e reasonError) Unwrap() error { return e.err }

// withReason is an error with a reason; InvalidSpec is never retried, any other one goes back to controller-runtime.
func withReason(reason string, err error) error { return reasonError{reason: reason, err: err} }

// waitFor is a normal wait: the condition says why and a watch re-triggers the reconcile.
func waitFor(reason string, err error) error {
	return reasonError{reason: reason, kind: kindWait, err: err}
}

// retryLater is a NetBox state no watch can see: the condition says why and the object is requeued.
func retryLater(reason string, err error) error {
	return reasonError{reason: reason, kind: kindRetry, err: err}
}

// ownedByOtherOwner fails if an object at path carries ref but is not o's: OwnedByOtherOwner under another owner,
// as after a move to a provider with another --netbox-owner, NotOwned without one. It guards the creates of a new ref.
func ownedByOtherOwner(ctx context.Context, nb *netbox.Client, o netbox.Owner, path, ref string) error {
	objs, err := nb.FindAnyByRef(ctx, path, ref)
	if err != nil {
		return err
	}
	for _, obj := range objs {
		switch {
		case o.Owns(obj):
		case obj.Owner == nil:
			return retryLater(v1alpha1.ReasonNotOwned, fmt.Errorf("NetBox %s%d carries %s without an owner", path, obj.ID, ref))
		default:
			return retryLater(v1alpha1.ReasonOwnedByOtherOwner, fmt.Errorf("NetBox %s%d carries %s under another owner; move and pivot need the same --netbox-owner", path, obj.ID, ref))
		}
	}
	return nil
}

// apiError is a failed Kubernetes API call.
func apiError(err error) error { return withReason(v1alpha1.ReasonInternalError, err) }

// asReason finds the reason of err; a NetBox 403 anywhere is retryLater(Forbidden).
func asReason(err error) (reasonError, bool) {
	var re reasonError
	switch {
	case errors.As(err, &re):
	case errors.Is(err, netbox.ErrForbidden):
		re = reasonError{reason: v1alpha1.ReasonForbidden, kind: kindRetry, err: err}
	default:
		return re, false
	}
	return re, true
}

// toResult turns the error of a reconcile into its result: only unexpected errors reach controller-runtime.
func toResult(err error) (ctrl.Result, error) {
	re, ok := asReason(err)
	switch {
	case !ok:
		return ctrl.Result{}, err
	case re.kind == kindWait, re.reason == v1alpha1.ReasonInvalidSpec:
		return ctrl.Result{}, nil
	case re.kind == kindRetry:
		return ctrl.Result{RequeueAfter: netBoxRequeueAfter}, nil
	}
	return ctrl.Result{}, err
}

func setReady(conds *[]metav1.Condition, generation int64, err error) {
	setCondition(conds, v1alpha1.ReadyCondition, v1alpha1.ReasonReady, generation, err)
}

// setDeleting reports a deletion: Deleting while it goes its way, the reason of err while it is stuck.
func setDeleting(conds *[]metav1.Condition, generation int64, err error) {
	c := metav1.Condition{Type: v1alpha1.ReadyCondition, Status: metav1.ConditionFalse, Reason: v1alpha1.ReasonDeleting, ObservedGeneration: generation}
	if err != nil {
		c.Reason, c.Message = errReason(err), err.Error()
	}
	meta.SetStatusCondition(conds, c)
}

func setCondition(conds *[]metav1.Condition, condType, okReason string, generation int64, err error) {
	c := metav1.Condition{Type: condType, Status: metav1.ConditionTrue, Reason: okReason, ObservedGeneration: generation}
	if err != nil {
		c.Status, c.Reason, c.Message = metav1.ConditionFalse, errReason(err), err.Error()
	}
	meta.SetStatusCondition(conds, c)
}

func errReason(err error) string {
	if re, ok := asReason(err); ok {
		return re.reason
	}
	return v1alpha1.ReasonNetBoxError
}

// ignoreGone is a client that does not fail a patch of a deleting object that is already gone:
// once a finalizer is removed, a stale cache can still run one more reconcile on the object.
// Any other NotFound is a real error and stays.
type ignoreGone struct{ client.Client }

func (c ignoreGone) Patch(ctx context.Context, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
	return goneWhenDeleting(obj, c.Client.Patch(ctx, obj, p, opts...))
}

func (c ignoreGone) Status() client.SubResourceWriter { return ignoreGoneStatus{c.Client.Status()} }

type ignoreGoneStatus struct{ client.SubResourceWriter }

func (s ignoreGoneStatus) Patch(ctx context.Context, obj client.Object, p client.Patch, opts ...client.SubResourcePatchOption) error {
	return goneWhenDeleting(obj, s.SubResourceWriter.Patch(ctx, obj, p, opts...))
}

func goneWhenDeleting(obj client.Object, err error) error {
	if apierrors.IsNotFound(err) && !obj.GetDeletionTimestamp().IsZero() {
		return nil
	}
	return err
}

// deletedForMove reports whether clusterctl move is deleting obj from the source cluster.
// Its NetBox objects live on for the target cluster, so a deletion like that must not release them.
func deletedForMove(o client.Object) bool {
	_, ok := o.GetAnnotations()[clusterctlv1.DeleteForMoveAnnotation]
	return ok
}

const poolRefField = "spec.poolRef.netboxIPPool"

var poolGK = metav1.GroupKind{Group: v1alpha1.GroupVersion.Group, Kind: v1alpha1.NetBoxIPPoolKind}

// poolRefName indexes claims and addresses by the NetBoxIPPool they point at.
func poolRefName(ref ipamv1.IPPoolReference) []string {
	if ref.APIGroup == poolGK.Group && ref.Kind == poolGK.Kind {
		return []string{ref.Name}
	}
	return nil
}
