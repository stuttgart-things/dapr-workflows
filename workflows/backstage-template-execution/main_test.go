package main

import (
	"reflect"
	"testing"
)

func TestResolveStringValue_Interpolation(t *testing.T) {
	exports := map[string]any{
		"vm.vm_ip":           "10.1.2.3",
		"ansible.kubeconfig": "secrets/kubeconfigs/foo.yaml",
	}
	cases := []struct {
		in      string
		want    interface{}
		wantErr bool
	}{
		{"plain", "plain", false},
		{"${stages.vm.vm_ip}", "10.1.2.3", false},
		{"prefix-${stages.vm.vm_ip}-suffix", "prefix-10.1.2.3-suffix", false},
		{"${stages.vm.vm_ip}/${stages.ansible.kubeconfig}", "10.1.2.3/secrets/kubeconfigs/foo.yaml", false},
		{"${stages.unknown.x}", nil, true},
		{"${stages.vm.vm_ip", nil, true},
	}
	for _, c := range cases {
		got, err := resolveStringValue(c.in, exports)
		if c.wantErr {
			if err == nil {
				t.Errorf("resolveStringValue(%q): want error, got %v", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("resolveStringValue(%q): unexpected error %v", c.in, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("resolveStringValue(%q): got %#v, want %#v", c.in, got, c.want)
		}
	}
}

func TestResolveStringValue_TypePreservation(t *testing.T) {
	// Whole-string placeholder must preserve the exported value's native type.
	exports := map[string]any{
		"vm.vm_ips":   []interface{}{"10.1.2.3", "10.1.2.4"},
		"vm.vm_count": float64(2),
		"vm.ready":    true,
	}
	cases := []struct {
		in   string
		want interface{}
	}{
		{"${stages.vm.vm_ips}", []interface{}{"10.1.2.3", "10.1.2.4"}},
		{"${stages.vm.vm_count}", float64(2)},
		{"${stages.vm.ready}", true},
		// Embedded => stringified
		{"count=${stages.vm.vm_count}", "count=2"},
		{"ready=${stages.vm.ready}", "ready=true"},
		// Embedded list stringifies via fmt %v.
		{"hosts=${stages.vm.vm_ips}", "hosts=[10.1.2.3 10.1.2.4]"},
	}
	for _, c := range cases {
		got, err := resolveStringValue(c.in, exports)
		if err != nil {
			t.Errorf("resolveStringValue(%q): unexpected error %v", c.in, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("resolveStringValue(%q): got %#v, want %#v", c.in, got, c.want)
		}
	}
}

func TestResolveValuesNested(t *testing.T) {
	exports := map[string]any{
		"vm.vm_ip":  "10.1.2.3",
		"vm.vm_ips": []interface{}{"10.1.2.3", "10.1.2.4"},
	}
	in := map[string]interface{}{
		"name":    "ansible",
		"targets": "${stages.vm.vm_ips}", // whole-string list export → expanded to list
		"first":   "${stages.vm.vm_ip}",  // whole-string scalar → preserved
		"label":   "deployed-${stages.vm.vm_ip}",
		"nested": map[string]interface{}{
			"host": "${stages.vm.vm_ip}",
			"port": float64(22),
		},
		"untouched_int":  42,
		"untouched_bool": true,
	}
	want := map[string]interface{}{
		"name":    "ansible",
		"targets": []interface{}{"10.1.2.3", "10.1.2.4"},
		"first":   "10.1.2.3",
		"label":   "deployed-10.1.2.3",
		"nested": map[string]interface{}{
			"host": "10.1.2.3",
			"port": float64(22),
		},
		"untouched_int":  42,
		"untouched_bool": true,
	}
	got, err := resolveValues(in, exports)
	if err != nil {
		t.Fatalf("resolveValues: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("resolveValues:\ngot:  %#v\nwant: %#v", got, want)
	}
}

func TestResolveValuesNoExportsNeeded(t *testing.T) {
	// Case (a) shape: no placeholders, empty exports map. Must pass through unchanged.
	in := map[string]interface{}{
		"vm_name":                  "dapr-vm-20260425",
		"vm_count":                 "1",
		"baseos_manage_filesystem": true,
	}
	got, err := resolveValues(in, map[string]any{})
	if err != nil {
		t.Fatalf("resolveValues: %v", err)
	}
	if !reflect.DeepEqual(got, in) {
		t.Errorf("expected pass-through, got %#v", got)
	}
}
