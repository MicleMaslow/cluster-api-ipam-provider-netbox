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

package main

import (
	"strings"
	"testing"

	"github.com/spf13/pflag"

	"github.com/MicleMaslow/cluster-api-ipam-provider-netbox/internal/netbox"
)

// parse runs the NetBox flags over args on a fresh flag set.
func parse(t *testing.T, args ...string) (netbox.Keys, error) {
	t.Helper()
	instanceName, refField = "", ""
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	initFlags(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	return netboxSettings()
}

func TestNetBoxFlagsAsManifestsRenderThem(t *testing.T) {
	k, err := parse(t, "--instance-name= ", "--netbox-ref-field=capi_ref")
	if err != nil || k != netbox.DefaultKeys {
		t.Errorf("default flags: %+v, %v; want the default keys", k, err)
	}
	k, err = parse(t, "--instance-name= ", "--netbox-ref-field= ")
	if err != nil || k != netbox.DefaultKeys {
		t.Errorf("blank flags, as the manifests render unset variables: %+v, %v", k, err)
	}
	k, err = parse(t, "--instance-name=zone-a", "--netbox-ref-field=ref")
	if want := (netbox.Keys{Instance: "zone-a", Ref: "ref"}); err != nil || k != want {
		t.Errorf("got %+v, %v; want %+v", k, err, want)
	}
	if k, err = parse(t); err != nil || k != netbox.DefaultKeys {
		t.Errorf("no flags: %+v, %v", k, err)
	}
	if k, err = parse(t, "--netbox-ref-field="); err != nil || k.Ref != netbox.DefaultKeys.Ref {
		t.Errorf("empty ref field: %+v, %v; want the default", k, err)
	}
	if k, err = parse(t, "--netbox-ref-field=A"); err != nil || k.Ref != "A" {
		t.Errorf("ref field A: %+v, %v; want it accepted", k, err)
	}
}

func TestInstanceNameFlag(t *testing.T) {
	for v, want := range map[string]string{"": "", " ": "", "a": "a", "zone-1": "zone-1", strings.Repeat("a", 63): strings.Repeat("a", 63)} {
		if k, err := parse(t, "--instance-name="+v); err != nil || k.Instance != want {
			t.Errorf("%q: %+v, %v; want instance %q", v, k, err, want)
		}
	}
	for _, v := range []string{"A_b", "a/b", "-a", "a.b", strings.Repeat("a", 64)} {
		if _, err := parse(t, "--instance-name="+v); err == nil || !strings.Contains(err.Error(), "instance name") {
			t.Errorf("%q: got %v, want an instance name error", v, err)
		}
	}
}

func TestNetBoxFlagsRejected(t *testing.T) {
	for want, args := range map[string][]string{
		"ref field": {"--netbox-ref-field=a__b"},
		"name must": {"--netbox-ref-field=a.b"},
	} {
		if _, err := parse(t, args...); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%v: got %v, want error with %q", args, err, want)
		}
	}
}
