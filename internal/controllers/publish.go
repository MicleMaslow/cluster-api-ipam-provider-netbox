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
	"bytes"
	"context"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/api/v1alpha1"
	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/internal/layout"
)

// maxPublishedSecrets is the number of Secrets status.publishedSecrets holds.
const maxPublishedSecrets = 16

// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;create;update;delete

// publishData is what the enabled pools and groups of nw publish, by Secret name, then key (the name of the pool or
// group): the addresses of a pool's ranges but for live, its addresses in use, and the VIP; the addresses of a
// group. Ascending, one per line, no mask, no trailing newline.
func publishData(nw *v1alpha1.NetBoxNetwork, l layout.Layout, live map[string][]netip.Addr) map[string]map[string][]byte {
	vip, _ := netip.ParseAddr(nw.Status.ControlPlaneEndpoint.Host)
	out := map[string]map[string][]byte{}
	put := func(p v1alpha1.Publish, key string, addrs []netip.Addr) {
		if !ptr.Deref(p.Enabled, false) {
			return
		}
		slices.SortFunc(addrs, netip.Addr.Compare)
		lines := make([]string, len(addrs))
		for i, a := range addrs {
			lines[i] = a.String()
		}
		if out[p.SecretName] == nil {
			out[p.SecretName] = map[string][]byte{}
		}
		out[p.SecretName][key] = []byte(strings.Join(lines, "\n"))
	}
	for _, sp := range nw.Spec.Pools {
		i := slices.IndexFunc(l.Pools, func(p layout.PlacedPool) bool { return p.Name == sp.Name })
		if i < 0 {
			continue
		}
		var addrs []netip.Addr
		for _, rng := range l.Pools[i].Ranges {
			for a := rng.From; a.IsValid() && !rng.To.Less(a); a = a.Next() {
				if a != vip && !slices.Contains(live[sp.Name], a) {
					addrs = append(addrs, a)
				}
			}
		}
		put(sp.Publish, sp.Name, addrs)
	}
	for _, sg := range nw.Spec.Addresses {
		if i := slices.IndexFunc(l.Groups, func(g layout.PlacedGroup) bool { return g.Name == sg.Name }); i >= 0 {
			put(sg.Publish, sg.Name, slices.Clone(l.Groups[i].Addrs))
		}
	}
	return out
}

// publish brings the Secrets of nw to data: each is written, as Opaque and controlled by nw, only when its data (or
// the UID of its owner reference) differs. The Secrets in status.publishedSecrets that data no longer has are deleted.
// A Secret that nw does not control is SecretConflict: it is not changed, nor recorded in the status. Secrets are read
// past the cache (APIReader), so the provider neither lists nor watches them.
func (r *NetBoxNetworkReconciler) publish(ctx context.Context, nw *v1alpha1.NetBoxNetwork, data map[string]map[string][]byte) error {
	if len(data) > maxPublishedSecrets {
		return withReason(v1alpha1.ReasonInvalidSpec, fmt.Errorf("pools and addresses publish in %d Secrets, at most %d", len(data), maxPublishedSecrets))
	}
	var (
		errs []error
		kept = map[string]bool{}
	)
	for _, name := range slices.Sorted(maps.Keys(data)) {
		err := r.publishSecret(ctx, nw, name, data[name])
		if err == nil || errReason(err) != v1alpha1.ReasonSecretConflict && slices.Contains(nw.Status.PublishedSecrets, name) {
			kept[name] = true
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	for _, name := range nw.Status.PublishedSecrets {
		if _, ok := data[name]; ok {
			continue
		}
		if err := r.deleteSecret(ctx, nw, name); err != nil {
			kept[name] = true
			errs = append(errs, err)
		}
	}
	nw.Status.PublishedSecrets = slices.Sorted(maps.Keys(kept))
	return foldErrors(errs)
}

// publishSecret creates the Secret name of nw with data, or brings the one it controls to it.
func (r *NetBoxNetworkReconciler) publishSecret(ctx context.Context, nw *v1alpha1.NetBoxNetwork, name string, data map[string][]byte) error {
	log := ctrl.LoggerFrom(ctx)
	cur := &corev1.Secret{}
	err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: nw.Namespace, Name: name}, cur)
	switch {
	case apierrors.IsNotFound(err):
		s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: nw.Namespace, Name: name}, Type: corev1.SecretTypeOpaque, Data: data}
		if err := controllerutil.SetControllerReference(nw, s, r.Scheme()); err != nil {
			return err
		}
		if err := r.Create(ctx, s); err != nil {
			return apiError(err)
		}
		log.Info("Created the Secret of published addresses", "network", klog.KObj(nw), "secret", klog.KObj(s), "keys", slices.Sorted(maps.Keys(data)))
		normalEvent(r.Recorder, nw, "SecretPublished", "Created the Secret %s", name)
		return nil
	case err != nil:
		return apiError(err)
	case !managedBy(cur, nw):
		return retryLater(v1alpha1.ReasonSecretConflict, fmt.Errorf("secret %s exists and is not managed by network %s", name, nw.Name))
	}
	ref := metav1.GetControllerOfNoCopy(cur)
	if ref.UID == nw.UID && maps.EqualFunc(cur.Data, data, bytes.Equal) {
		return nil
	}
	ref.UID, cur.Data = nw.UID, data // a stale UID left, the garbage collector would delete the Secret
	if err := r.Update(ctx, cur); err != nil {
		return apiError(err)
	}
	log.Info("Updated the Secret of published addresses", "network", klog.KObj(nw), "secret", klog.KObj(cur), "keys", slices.Sorted(maps.Keys(data)))
	normalEvent(r.Recorder, nw, "SecretPublished", "Updated the Secret %s", name)
	return nil
}

// deleteSecret deletes the Secret name if nw controls it.
func (r *NetBoxNetworkReconciler) deleteSecret(ctx context.Context, nw *v1alpha1.NetBoxNetwork, name string) error {
	cur := &corev1.Secret{}
	switch err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: nw.Namespace, Name: name}, cur); {
	case apierrors.IsNotFound(err):
		return nil
	case err != nil:
		return apiError(err)
	case !managedBy(cur, nw):
		return nil
	}
	if err := client.IgnoreNotFound(r.Delete(ctx, cur, client.Preconditions{UID: &cur.UID, ResourceVersion: &cur.ResourceVersion})); err != nil {
		return apiError(err)
	}
	ctrl.LoggerFrom(ctx).Info("Deleted the Secret of published addresses", "network", klog.KObj(nw), "secret", klog.KObj(cur))
	normalEvent(r.Recorder, nw, "SecretDeleted", "Deleted the Secret %s: no pool or address group publishes in it", name)
	return nil
}
