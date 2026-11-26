package editor

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jollaman999/give-me-real-gpu/internal/topology"
)

func do(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, req)
	return rec
}

func TestCatalog(t *testing.T) {
	rec := do(t, http.MethodGet, CatalogPath, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content type %q", ct)
	}
	var c topology.Catalog
	if err := json.Unmarshal(rec.Body.Bytes(), &c); err != nil {
		t.Fatal(err)
	}
	if len(c.GPUModels) != 6 || c.GPUModels[0].Name != "NVIDIA A10" || c.Defaults.MaxMIGSlices != 7 {
		t.Fatalf("catalog %+v", c)
	}
}

func TestFill(t *testing.T) {
	body := `{"hypervisors":[{"hostname":"n1","mode":"PASSTHROUGH","gpus":[
		{"index":0,"model_name":"NVIDIA L40S","vms":[{"name":"v1","mig_gpu_instance_id":-1}]}]}]}`
	rec := do(t, http.MethodPost, FillPath, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body)
	}
	var resp FillResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Problems) != 0 {
		t.Fatalf("problems %v", resp.Problems)
	}
	vm := resp.Topology.Hypervisors[0].GPUs[0].VMs[0]
	if vm.ID != topology.StableUUID("server", "v1") || vm.IP != "198.51.100.11" {
		t.Fatalf("vm not filled %+v", vm)
	}
	if f := resp.Topology.FlavorByID(vm.FlavorID); f == nil || f.Name != "emu-pt-l40s" {
		t.Fatalf("flavor %+v", f)
	}
	if !strings.Contains(rec.Body.String(), `"problems":[]`) {
		t.Fatalf("problems must be an empty array, got %s", rec.Body)
	}
}

func TestFillProblems(t *testing.T) {
	// two VMs on one passthrough GPU and a VM with a gpu instance id on a non-MIG node.
	body := `{"hypervisors":[{"hostname":"n1","mode":"PASSTHROUGH","gpus":[
		{"index":0,"model_name":"NVIDIA L40S","vms":[{"name":"v1","mig_gpu_instance_id":-1},{"name":"v2","mig_gpu_instance_id":3}]}]}]}`
	rec := do(t, http.MethodPost, FillPath, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var resp FillResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"hypervisor n1 gpu 0: passthrough GPU has 2 VMs (max 1)",
		"vm v2: mig_gpu_instance_id is 3 on a non-MIG node (must be -1)",
	}
	if strings.Join(resp.Problems, "|") != strings.Join(want, "|") {
		t.Fatalf("problems %q, want %q", resp.Problems, want)
	}
}

func TestFillBadBody(t *testing.T) {
	for _, body := range []string{`{"hypervisors":[{"hostnam":"x"}]}`, `{}{}`, `nope`} {
		rec := do(t, http.MethodPost, FillPath, body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d", body, rec.Code)
			continue
		}
		var e errorBody
		if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || e.Error != "invalid request body" || len(e.Problems) != 1 {
			t.Errorf("%s: body %s", body, rec.Body)
		}
	}
}

func TestRoutes(t *testing.T) {
	if rec := do(t, http.MethodGet, FillPath, ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET fill: %d", rec.Code)
	}
	if rec := do(t, http.MethodPost, CatalogPath, ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST catalog: %d", rec.Code)
	}
	if rec := do(t, http.MethodGet, "/api/v1/topology", ""); rec.Code != http.StatusNotFound {
		t.Errorf("GET topology: %d", rec.Code)
	}
}
