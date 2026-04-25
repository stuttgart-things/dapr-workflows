package main

import (
	"reflect"
	"testing"
)

func TestResolveString(t *testing.T) {
	exports := map[string]string{
		"vm.vm_ip":            "10.1.2.3",
		"ansible.kubeconfig":  "secrets/kubeconfigs/foo.yaml",
	}
	cases := []struct {
		in, want string
		wantErr  bool
	}{
		{"plain", "plain", false},
		{"${stages.vm.vm_ip}", "10.1.2.3", false},
		{"prefix-${stages.vm.vm_ip}-suffix", "prefix-10.1.2.3-suffix", false},
		{"${stages.vm.vm_ip}/${stages.ansible.kubeconfig}", "10.1.2.3/secrets/kubeconfigs/foo.yaml", false},
		{"${stages.unknown.x}", "", true},
		{"${stages.vm.vm_ip", "", true},
	}
	for _, c := range cases {
		got, err := resolveString(c.in, exports)
		if c.wantErr {
			if err == nil {
				t.Errorf("resolveString(%q): want error, got %q", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("resolveString(%q): unexpected error %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("resolveString(%q): got %q, want %q", c.in, got, c.want)
		}
	}
}

func TestResolveValuesNested(t *testing.T) {
	exports := map[string]string{"vm.vm_ip": "10.1.2.3"}
	in := map[string]interface{}{
		"name": "ansible",
		"targets": []interface{}{
			"${stages.vm.vm_ip}",
			"static-host",
		},
		"nested": map[string]interface{}{
			"host": "${stages.vm.vm_ip}",
			"port": float64(22), // json.Unmarshal turns numbers into float64
		},
		"untouched_int":  42,
		"untouched_bool": true,
	}
	want := map[string]interface{}{
		"name": "ansible",
		"targets": []interface{}{
			"10.1.2.3",
			"static-host",
		},
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
		"vm_name": "dapr-vm-20260425",
		"vm_count": "1",
		"baseos_manage_filesystem": true,
	}
	got, err := resolveValues(in, map[string]string{})
	if err != nil {
		t.Fatalf("resolveValues: %v", err)
	}
	if !reflect.DeepEqual(got, in) {
		t.Errorf("expected pass-through, got %#v", got)
	}
}
