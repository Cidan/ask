package filters

import (
	"strings"
	"testing"
)

// A `-o yaml` dump has its managedFields block dropped, keeping every other
// field of the resource.
func TestKubectl_StripsYAMLManagedFields(t *testing.T) {
	raw := `apiVersion: v1
kind: ConfigMap
metadata:
  name: my-config
  namespace: default
  managedFields:
  - apiVersion: v1
    fieldsType: FieldsV1
    fieldsV1:
      f:data:
        f:key: {}
    manager: kubectl
    operation: Update
  resourceVersion: "12345"
data:
  key: value
`
	out, saved := Apply("kubectl get configmap my-config -o yaml", raw, 0)
	if !strings.Contains(out, "managedFields: [... 7 lines omitted to save tokens ...]") {
		t.Fatalf("managedFields not collapsed: %q", out)
	}
	if strings.Contains(out, "fieldsType") || strings.Contains(out, "manager: kubectl") {
		t.Errorf("managedFields content survived: %q", out)
	}
	for _, keep := range []string{"name: my-config", `resourceVersion: "12345"`, "key: value"} {
		if !strings.Contains(out, keep) {
			t.Errorf("dropped a real field %q: %q", keep, out)
		}
	}
	if saved <= 0 {
		t.Errorf("expected savings, got %d", saved)
	}
}

// A `-o json` dump has its managedFields array dropped, keeping the rest.
func TestKubectl_StripsJSONManagedFields(t *testing.T) {
	raw := `{
    "apiVersion": "v1",
    "kind": "ConfigMap",
    "metadata": {
        "name": "my-config",
        "managedFields": [
            {
                "apiVersion": "v1",
                "manager": "kubectl"
            }
        ],
        "resourceVersion": "12345"
    },
    "data": {"key": "value"}
}
`
	out, _ := Apply("kubectl get configmap my-config -o json", raw, 0)
	if !strings.Contains(out, `"managedFields": [... 5 lines omitted to save tokens ...],`) {
		t.Fatalf("json managedFields not collapsed: %q", out)
	}
	if strings.Contains(out, `"manager": "kubectl"`) {
		t.Errorf("managedFields content survived: %q", out)
	}
	for _, keep := range []string{`"resourceVersion": "12345"`, `"data": {"key": "value"}`} {
		if !strings.Contains(out, keep) {
			t.Errorf("dropped a real field %q: %q", keep, out)
		}
	}
}

// kubectl's own deprecation/skew warnings are stripped; the table stays.
func TestKubectl_StripsWarnings(t *testing.T) {
	raw := strings.Join([]string{
		"Warning: extensions/v1beta1 Ingress is deprecated in v1.14+, unavailable in v1.22+; use networking.k8s.io/v1 Ingress",
		"NAME    CLASS   HOSTS   ADDRESS   PORTS   AGE",
		"web     nginx   x.com   1.2.3.4   80      5d",
	}, "\n") + "\n"
	out, _ := Apply("kubectl get ingress", raw, 0)
	if strings.Contains(out, "is deprecated") {
		t.Errorf("deprecation warning survived: %q", out)
	}
	if !strings.Contains(out, "NAME    CLASS") || !strings.Contains(out, "web     nginx") {
		t.Errorf("table dropped: %q", out)
	}
}

// A failed kubectl command is returned verbatim — the error is the point,
// and managedFields-looking lines are not touched on failure.
func TestKubectl_FailurePreserved(t *testing.T) {
	raw := "Error from server (NotFound): pods \"x\" not found\nmanagedFields:\n  - foo\n"
	out, _ := Apply("kubectl get pod x -o yaml", raw, 1)
	if !strings.Contains(out, "Error from server (NotFound)") {
		t.Errorf("error dropped: %q", out)
	}
	if !strings.Contains(out, "managedFields:") {
		t.Errorf("managedFields stripped on failure: %q", out)
	}
}

// A plain get table with no managedFields passes through untouched.
func TestKubectl_PlainTablePassesThrough(t *testing.T) {
	raw := "NAME    READY   STATUS    RESTARTS   AGE\npod-1   1/1     Running   0          5d\n"
	if out, _ := Apply("kubectl get pods", raw, 0); out != raw {
		t.Errorf("plain table altered: %q", out)
	}
}
