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

package controllers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/api/v1alpha1"
	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/internal/netbox"
)

func TestOwnedByOtherOwner(t *testing.T) {
	for name, tc := range map[string]struct {
		owner, reason, message string
	}{
		"ours":     {`{"id":1}`, "", ""},
		"other":    {`{"id":2}`, v1alpha1.ReasonOwnedByOtherOwner, "under another owner"},
		"no owner": {`null`, v1alpha1.ReasonNotOwned, "without an owner"},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprintf(w, `{"results":[{"id":5,"owner":%s,"custom_fields":{"capi_ref":"x/a"}}]}`, tc.owner)
		}))
		err := ownedByOtherOwner(ctx, &netbox.Client{URL: srv.URL, Token: "t"}, netbox.Owner{ID: 1}, netbox.Prefixes, "x/a")
		srv.Close()
		if tc.reason == "" {
			if err != nil {
				t.Errorf("%s: err %v", name, err)
			}
			continue
		}
		if errReason(err) != tc.reason || !strings.Contains(fmt.Sprint(err), tc.message) {
			t.Errorf("%s: err %v (reason %s), want %s with %q", name, err, errReason(err), tc.reason, tc.message)
		}
	}
}

func TestToResult(t *testing.T) {
	boom := errors.New("boom")
	forbidden := fmt.Errorf("DELETE ipam/prefixes/1/: %w", netbox.ErrForbidden)
	internal := withReason(v1alpha1.ReasonInternalError, boom)
	for name, tc := range map[string]struct {
		err     error
		want    ctrl.Result
		wantErr error
		reason  string
	}{
		"waitFor":              {waitFor(v1alpha1.ReasonNetworkNotReady, boom), ctrl.Result{}, nil, v1alpha1.ReasonNetworkNotReady},
		"waitFor wrapped":      {fmt.Errorf("ctx: %w", waitFor(v1alpha1.ReasonWaitingForPools, boom)), ctrl.Result{}, nil, v1alpha1.ReasonWaitingForPools},
		"retryLater":           {retryLater(v1alpha1.ReasonParentNotFound, boom), ctrl.Result{RequeueAfter: time.Minute}, nil, v1alpha1.ReasonParentNotFound},
		"invalid spec":         {withReason(v1alpha1.ReasonInvalidSpec, boom), ctrl.Result{}, nil, v1alpha1.ReasonInvalidSpec},
		"internal error":       {internal, ctrl.Result{}, internal, v1alpha1.ReasonInternalError},
		"plain error":          {boom, ctrl.Result{}, boom, v1alpha1.ReasonNetBoxError},
		"forbidden":            {forbidden, ctrl.Result{RequeueAfter: time.Minute}, nil, v1alpha1.ReasonForbidden},
		"forbidden aggregated": {kerrors.NewAggregate([]error{boom, forbidden}), ctrl.Result{RequeueAfter: time.Minute}, nil, v1alpha1.ReasonForbidden},
		"nil":                  {nil, ctrl.Result{}, nil, v1alpha1.ReasonReady},
	} {
		got, err := toResult(tc.err)
		if got != tc.want || !errors.Is(err, tc.wantErr) || (tc.wantErr == nil) != (err == nil) {
			t.Errorf("%s: toResult = %+v, %v; want %+v, %v", name, got, err, tc.want, tc.wantErr)
		}
		var conds []metav1.Condition
		setReady(&conds, 1, tc.err)
		if c := meta.FindStatusCondition(conds, v1alpha1.ReadyCondition); c == nil || c.Reason != tc.reason {
			t.Errorf("%s: Ready condition %+v, want reason %s", name, c, tc.reason)
		}
	}
}

func TestSetDeleting(t *testing.T) {
	for name, tc := range map[string]struct {
		err    error
		reason string
	}{
		"normal course": {nil, v1alpha1.ReasonDeleting},
		"blocked":       {waitFor(v1alpha1.ReasonWaitingForAddresses, errors.New("2 addresses")), v1alpha1.ReasonWaitingForAddresses},
		"forbidden":     {fmt.Errorf("DELETE: %w", netbox.ErrForbidden), v1alpha1.ReasonForbidden},
	} {
		conds := []metav1.Condition{{Type: v1alpha1.ReadyCondition, Status: metav1.ConditionTrue, Reason: v1alpha1.ReasonReady}}
		setDeleting(&conds, 3, tc.err)
		c := meta.FindStatusCondition(conds, v1alpha1.ReadyCondition)
		if c.Status != metav1.ConditionFalse || c.Reason != tc.reason || c.ObservedGeneration != 3 {
			t.Errorf("%s: %+v, want False/%s", name, c, tc.reason)
		}
	}
}

func TestIgnoreGone(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	failing := interceptor.Funcs{
		Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
			return boom
		},
		SubResourcePatch: func(context.Context, client.Client, string, client.Object, client.Patch, ...client.SubResourcePatchOption) error {
			return boom
		},
	}
	deleting := newNetwork("ns", "gone")
	deleting.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	raw := client.RawPatch(types.MergePatchType, []byte(`{}`))
	for name, tc := range map[string]struct {
		obj   client.Object
		funcs interceptor.Funcs
		want  func(error) bool
	}{
		"not found, object alive":    {newNetwork("ns", "gone"), interceptor.Funcs{}, apierrors.IsNotFound},
		"other error":                {newNetwork("ns", "gone"), failing, func(err error) bool { return errors.Is(err, boom) }},
		"not found, object deleting": {deleting, interceptor.Funcs{}, func(err error) bool { return err == nil }},
	} {
		c := ignoreGone{fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(tc.funcs).Build()}
		if err := c.Patch(context.Background(), tc.obj, raw); !tc.want(err) {
			t.Errorf("%s: Patch: %v", name, err)
		}
		if err := c.Status().Patch(context.Background(), tc.obj, raw); !tc.want(err) {
			t.Errorf("%s: Status().Patch: %v", name, err)
		}
	}
}

func TestNetworkUsable(t *testing.T) {
	cond := func(status metav1.ConditionStatus, reason string) []metav1.Condition {
		return []metav1.Condition{{Type: v1alpha1.ReadyCondition, Status: status, Reason: reason}}
	}
	for name, tc := range map[string]struct {
		prefix   string
		conds    []metav1.Condition
		deleting bool
		want     bool
	}{
		"ready":                    {"10.10.0.0/28", cond(metav1.ConditionTrue, v1alpha1.ReasonReady), false, true},
		"ready without prefix":     {"", cond(metav1.ConditionTrue, v1alpha1.ReasonReady), false, false},
		"no condition":             {"10.10.0.0/28", nil, false, false},
		"not ready":                {"10.10.0.0/28", cond(metav1.ConditionFalse, v1alpha1.ReasonParentNotFound), false, false},
		"deleting, blocked":        {"10.10.0.0/28", cond(metav1.ConditionFalse, v1alpha1.ReasonWaitingForPools), true, true},
		"deleting, stale ready":    {"10.10.0.0/28", cond(metav1.ConditionTrue, v1alpha1.ReasonReady), true, false},
		"deleting, going":          {"10.10.0.0/28", cond(metav1.ConditionFalse, v1alpha1.ReasonDeleting), true, false},
		"deleting, no prefix":      {"", cond(metav1.ConditionFalse, v1alpha1.ReasonWaitingForPools), true, false},
		"deleting, forbidden":      {"10.10.0.0/28", cond(metav1.ConditionFalse, v1alpha1.ReasonForbidden), true, false},
		"blocked reason, not gone": {"10.10.0.0/28", cond(metav1.ConditionFalse, v1alpha1.ReasonWaitingForPools), false, false},
	} {
		nw := newNetwork("ns", "n")
		nw.Status.Prefix, nw.Status.Conditions = tc.prefix, tc.conds
		if tc.deleting {
			nw.DeletionTimestamp = &metav1.Time{Time: time.Now()}
		}
		if got := networkUsable(nw); got != tc.want {
			t.Errorf("%s: networkUsable = %t, want %t", name, got, tc.want)
		}
	}
}
