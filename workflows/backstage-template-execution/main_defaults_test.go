package main

import (
	"encoding/json"
	"testing"
)

// The shape that matters, taken from create-terraform-vm: a plain page with a
// default, and a `dependencies.lab.oneOf` whose LabDA arm carries the fields
// that name an actual machine. The LabUL arm nests a further dependency.
const entityJSON = `{
  "spec": {
    "parameters": [
      {
        "title": "General",
        "properties": {
          "lab":      {"type": "string", "default": "LabUL", "enum": ["LabDA","LabUL"]},
          "vm_name":  {"type": "string"}
        },
        "dependencies": {
          "lab": {
            "oneOf": [
              {
                "properties": {
                  "lab":       {"enum": ["LabDA"]},
                  "datastore": {"type": "string", "default": "/DC/datastore/ds-01"},
                  "network":   {"type": "string", "default": "/DC/network/tiab-prod"}
                }
              },
              {
                "properties": {
                  "lab":   {"enum": ["LabUL"]},
                  "cloud": {"type": "string", "default": "proxmox", "enum": ["proxmox"]}
                },
                "dependencies": {
                  "cloud": {
                    "oneOf": [
                      {
                        "properties": {
                          "cloud":       {"enum": ["proxmox"]},
                          "pve_datastore": {"type": "string", "default": "V5010-01-1"}
                        }
                      }
                    ]
                  }
                }
              }
            ]
          }
        }
      },
      {
        "title": "Terraform Backend (S3)",
        "properties": {
          "s3_endpoint": {"type": "string", "default": "https://minio.example.com"},
          "s3_bucket":   {"type": "string", "default": "vsphere-labda"}
        }
      }
    ]
  }
}`

func entity(t *testing.T) map[string]interface{} {
	t.Helper()
	var e map[string]interface{}
	if err := json.Unmarshal([]byte(entityJSON), &e); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	return e
}

// The regression: an API-driven run supplies a handful of values and gets empty
// strings for everything else, because the scaffolder does not apply defaults.
func TestFillsUnconditionalDefaults(t *testing.T) {
	got := applySchemaDefaults(entity(t), map[string]interface{}{"vm_name": "labda-dev-a", "lab": "LabDA"})
	if got["s3_endpoint"] != "https://minio.example.com" {
		t.Fatalf("s3_endpoint not filled: %#v", got["s3_endpoint"])
	}
	if got["s3_bucket"] != "vsphere-labda" {
		t.Fatalf("s3_bucket not filled: %#v", got["s3_bucket"])
	}
}

// The half that actually built a broken VM: datastore and network live in the
// LabDA arm of a oneOf, so filling only top-level properties is not enough.
func TestFillsSelectedBranchDefaults(t *testing.T) {
	got := applySchemaDefaults(entity(t), map[string]interface{}{"lab": "LabDA"})
	if got["datastore"] != "/DC/datastore/ds-01" {
		t.Fatalf("datastore not filled from the LabDA arm: %#v", got["datastore"])
	}
	if got["network"] != "/DC/network/tiab-prod" {
		t.Fatalf("network not filled from the LabDA arm: %#v", got["network"])
	}
}

// The arm that does NOT apply must contribute nothing -- a LabDA run has no
// business carrying proxmox defaults.
func TestIgnoresUnselectedBranch(t *testing.T) {
	got := applySchemaDefaults(entity(t), map[string]interface{}{"lab": "LabDA"})
	if _, present := got["cloud"]; present {
		t.Fatalf("LabUL arm leaked into a LabDA run: cloud=%#v", got["cloud"])
	}
	if _, present := got["pve_datastore"]; present {
		t.Fatal("nested LabUL dependency leaked into a LabDA run")
	}
}

// Branches nest: choosing LabUL selects cloud=proxmox, which in turn selects
// its own arm.
func TestFollowsNestedDependency(t *testing.T) {
	got := applySchemaDefaults(entity(t), map[string]interface{}{"lab": "LabUL"})
	if got["cloud"] != "proxmox" {
		t.Fatalf("cloud not filled: %#v", got["cloud"])
	}
	if got["pve_datastore"] != "V5010-01-1" {
		t.Fatalf("nested default not filled: %#v", got["pve_datastore"])
	}
}

// An explicit value wins, including an explicit empty string: "I mean blank"
// and "I said nothing" are different, and only the second is being repaired.
func TestNeverOverwritesCaller(t *testing.T) {
	got := applySchemaDefaults(entity(t), map[string]interface{}{
		"lab":         "LabDA",
		"datastore":   "/DC/datastore/ds-99",
		"s3_endpoint": "",
	})
	if got["datastore"] != "/DC/datastore/ds-99" {
		t.Fatalf("caller value overwritten: %#v", got["datastore"])
	}
	if got["s3_endpoint"] != "" {
		t.Fatalf("explicit empty string overwritten: %#v", got["s3_endpoint"])
	}
}

// With no discriminator there is no arm to choose, and guessing one would be
// worse than leaving the fields out.
func TestNoBranchWithoutDiscriminator(t *testing.T) {
	got := applySchemaDefaults(entity(t), map[string]interface{}{"vm_name": "x"})
	if _, present := got["datastore"]; present {
		t.Fatal("picked a branch without a discriminator value")
	}
	if got["lab"] != "LabUL" {
		t.Fatalf("top-level default still applies: %#v", got["lab"])
	}
}
