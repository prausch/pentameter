package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newControlMonitor builds a monitor pre-populated with the registries a real poll would fill
// in, so resolution can be tested without an IntelliCenter.
func newControlMonitor() *PoolMonitor {
	pm := NewPoolMonitor("test", "6680", false)
	pm.registerBodyObject("Pool", "POOL", "B1101")
	pm.circuitNames["C0002"] = "Cleaner"
	pm.circuitNames["C0005"] = "Pool Light"
	pm.circuitNames["C0006"] = "Pool"
	pm.heaterObjects["H0001"] = "Solar Heater"
	return pm
}

func TestRegisterBodyObjectLookupKeys(t *testing.T) {
	pm := newControlMonitor()

	// A caller should be able to reach the body by friendly name, subtype, or objnam, in any
	// case — the Hubitat driver has no way to discover B1101 from /metrics.
	for _, ref := range []string{"Pool", "pool", "POOL", "B1101", "b1101"} {
		got, err := pm.resolveBody(ref)
		if err != nil {
			t.Fatalf("resolveBody(%q) returned error: %v", ref, err)
		}
		if got != "B1101" {
			t.Errorf("resolveBody(%q) = %q, want B1101", ref, got)
		}
	}
}

func TestResolveBodyUnknownListsKnown(t *testing.T) {
	pm := newControlMonitor()

	_, err := pm.resolveBody("spa")
	if err == nil {
		t.Fatal("expected an error for an unknown body")
	}
	// The error should be actionable — a caller that guessed wrong needs to see the valid set.
	if !strings.Contains(err.Error(), "b1101") {
		t.Errorf("error should list known bodies, got %q", err)
	}
}

func TestResolveBodyEmpty(t *testing.T) {
	pm := newControlMonitor()
	if _, err := pm.resolveBody(""); err == nil {
		t.Error("expected an error for an empty body reference")
	}
}

func TestResolveCircuitByObjnamAndName(t *testing.T) {
	pm := newControlMonitor()

	cases := map[string]string{
		"C0006":      "C0006",
		"C0002":      "C0002",
		"Pool Light": "C0005",
		"pool light": "C0005",
		"Cleaner":    "C0002",
	}
	for ref, want := range cases {
		got, err := pm.resolveCircuit(ref)
		if err != nil {
			t.Fatalf("resolveCircuit(%q) returned error: %v", ref, err)
		}
		if got != want {
			t.Errorf("resolveCircuit(%q) = %q, want %q", ref, got, want)
		}
	}
}

func TestResolveCircuitUnknown(t *testing.T) {
	pm := newControlMonitor()
	if _, err := pm.resolveCircuit("Spa Jets"); err == nil {
		t.Error("expected an error for an unknown circuit")
	}
}

func TestResolveHeaterSingleInstalled(t *testing.T) {
	pm := newControlMonitor()

	// With one heater the caller may omit the field entirely — this is the normal case here.
	got, err := pm.resolveHeater("")
	if err != nil {
		t.Fatalf("resolveHeater(\"\") returned error: %v", err)
	}
	if got != "H0001" {
		t.Errorf("resolveHeater(\"\") = %q, want H0001", got)
	}
}

func TestResolveHeaterAmbiguousRequiresChoice(t *testing.T) {
	pm := newControlMonitor()
	pm.heaterObjects["H0002"] = "Heat Pump"

	// Silently picking one of two heaters would actuate the wrong equipment, so this must fail
	// rather than guess.
	if _, err := pm.resolveHeater(""); err == nil {
		t.Fatal("expected an error when multiple heaters are present and none is specified")
	}

	got, err := pm.resolveHeater("Heat Pump")
	if err != nil {
		t.Fatalf("resolveHeater by name returned error: %v", err)
	}
	if got != "H0002" {
		t.Errorf("resolveHeater(\"Heat Pump\") = %q, want H0002", got)
	}
}

func TestResolveHeaterNoneKnownYet(t *testing.T) {
	pm := NewPoolMonitor("test", "6680", false)
	if _, err := pm.resolveHeater(""); err == nil {
		t.Error("expected an error before the first poll has discovered any heater")
	}
}

func TestNormalizeOnOff(t *testing.T) {
	on := []string{"on", "ON", "On", "1", "true", " on "}
	off := []string{"off", "OFF", "0", "false"}

	for _, v := range on {
		got, err := normalizeOnOff(v, "state")
		if err != nil || got != "ON" {
			t.Errorf("normalizeOnOff(%q) = %q, %v; want ON, nil", v, got, err)
		}
	}
	for _, v := range off {
		got, err := normalizeOnOff(v, "state")
		if err != nil || got != "OFF" {
			t.Errorf("normalizeOnOff(%q) = %q, %v; want OFF, nil", v, got, err)
		}
	}
	for _, v := range []string{"", "toggle", "yes"} {
		if _, err := normalizeOnOff(v, "state"); err == nil {
			t.Errorf("normalizeOnOff(%q) should have errored", v)
		}
	}
}

// applyCommand's validation must reject bad requests before reaching the WebSocket. These run
// against a monitor with no connection, so anything that gets as far as sendSetParamList
// surfaces as errNotConnected — which is itself the signal that validation let it through.
func TestApplyCommandValidation(t *testing.T) {
	setpoint := func(v float64) *float64 { return &v }

	tests := []struct {
		req      *CommandRequest
		name     string
		wantErr  string
		reachesW bool
	}{
		{
			name:    "no action",
			req:     &CommandRequest{},
			wantErr: "no action given",
		},
		{
			name:    "two actions",
			req:     &CommandRequest{Circuit: "C0006", State: "on", Heat: "on", Body: "pool"},
			wantErr: "exactly one action",
		},
		{
			name:    "circuit without state",
			req:     &CommandRequest{Circuit: "C0006"},
			wantErr: "state must be on or off",
		},
		{
			name:    "unknown circuit",
			req:     &CommandRequest{Circuit: "Spa Jets", State: "on"},
			wantErr: "unknown circuit",
		},
		{
			name:    "heat without body",
			req:     &CommandRequest{Heat: "on"},
			wantErr: "body is required",
		},
		{
			name:    "setpoint too low",
			req:     &CommandRequest{Body: "pool", Setpoint: setpoint(20)},
			wantErr: "out of range",
		},
		{
			name:    "setpoint too high",
			req:     &CommandRequest{Body: "pool", Setpoint: setpoint(140)},
			wantErr: "out of range",
		},
		{
			name:     "valid circuit reaches the wire",
			req:      &CommandRequest{Circuit: "C0006", State: "off"},
			reachesW: true,
		},
		{
			name:     "valid heat reaches the wire",
			req:      &CommandRequest{Body: "pool", Heat: "on"},
			reachesW: true,
		},
		{
			name:     "valid setpoint reaches the wire",
			req:      &CommandRequest{Body: "pool", Setpoint: setpoint(82)},
			reachesW: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pm := newControlMonitor()
			_, err := pm.applyCommand(tt.req)
			if err == nil {
				t.Fatal("applyCommand returned no error; it cannot succeed without a connection")
			}
			if tt.reachesW {
				if !strings.Contains(err.Error(), errNotConnected.Error()) {
					t.Errorf("expected the request to pass validation and fail at the wire, got %q", err)
				}
				return
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

// Setpoint bounds are inclusive at both ends.
func TestApplySetpointBoundsInclusive(t *testing.T) {
	for _, v := range []float64{minSetpointF, maxSetpointF} {
		pm := newControlMonitor()
		value := v
		_, err := pm.applyCommand(&CommandRequest{Body: "pool", Setpoint: &value})
		if err == nil || !strings.Contains(err.Error(), errNotConnected.Error()) {
			t.Errorf("setpoint %.0f should be accepted by validation, got %v", v, err)
		}
	}
}

func TestHandleCommandDisabledWithoutToken(t *testing.T) {
	t.Setenv(commandTokenEnv, "")
	pm := newControlMonitor()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/command", strings.NewReader(`{"circuit":"C0006","state":"off"}`))
	pm.handleCommand(rec, req)

	// Control is opt-in: with no token configured the route must not exist at all, rather than
	// running unauthenticated.
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestHandleCommandRejectsBadToken(t *testing.T) {
	t.Setenv(commandTokenEnv, "s3cret")
	pm := newControlMonitor()

	for _, token := range []string{"", "wrong", "s3cre", "s3cretx"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/command", strings.NewReader(`{"circuit":"C0006","state":"off"}`))
		if token != "" {
			req.Header.Set("X-Auth-Token", token)
		}
		pm.handleCommand(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("token %q: status = %d, want %d", token, rec.Code, http.StatusUnauthorized)
		}
	}
}

func TestHandleCommandRejectsNonPost(t *testing.T) {
	t.Setenv(commandTokenEnv, "s3cret")
	pm := newControlMonitor()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/command", nil)
	req.Header.Set("X-Auth-Token", "s3cret")
	pm.handleCommand(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
	if allow := rec.Header().Get("Allow"); allow != http.MethodPost {
		t.Errorf("Allow header = %q, want POST", allow)
	}
}

func TestHandleCommandRejectsBadJSON(t *testing.T) {
	t.Setenv(commandTokenEnv, "s3cret")
	pm := newControlMonitor()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/command", strings.NewReader(`{"circuit":`))
	req.Header.Set("X-Auth-Token", "s3cret")
	pm.handleCommand(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

// A command that passes auth and validation but arrives while the controller link is down must
// be distinguishable from a malformed request — the Hubitat driver should retry the former and
// not the latter.
func TestHandleCommandDisconnectedIsServiceUnavailable(t *testing.T) {
	t.Setenv(commandTokenEnv, "s3cret")
	pm := newControlMonitor()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/command", strings.NewReader(`{"circuit":"C0006","state":"off"}`))
	req.Header.Set("X-Auth-Token", "s3cret")
	pm.handleCommand(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}

	var resp CommandResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("response body is not valid JSON: %v", err)
	}
	if resp.Status != "error" || resp.Error == "" {
		t.Errorf("expected an error response, got %+v", resp)
	}
}

func TestHandleCommandValidationErrorIsBadRequest(t *testing.T) {
	t.Setenv(commandTokenEnv, "s3cret")
	pm := newControlMonitor()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/command", strings.NewReader(`{"body":"pool","setpoint":200}`))
	req.Header.Set("X-Auth-Token", "s3cret")
	pm.handleCommand(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

// The heat-off path must not depend on referencedHeaters, which drops the body as soon as
// HTSRC is cleared. Turning heat off and then on again is the exact sequence that would break
// if control resolved bodies through it.
func TestHeatOffThenOnKeepsBodyResolvable(t *testing.T) {
	pm := newControlMonitor()

	// Simulate a poll observing the body with heat already disabled.
	referenced := make(map[string]BodyHeaterInfo)
	pm.processBodyObject(ObjectData{
		ObjName: "B1101",
		Params: map[string]string{
			"SNAME": "Pool", "SUBTYP": "POOL", "TEMP": "74",
			"HTMODE": "0", "HTSRC": heatSourceOff, "LOTMP": "84", "HITMP": "104",
		},
	}, referenced)

	if len(referenced) != 0 {
		t.Fatal("precondition failed: a body with HTSRC=00000 should not be a referenced heater")
	}
	if _, err := pm.resolveBody("pool"); err != nil {
		t.Errorf("body must stay resolvable while heat is off, got %v", err)
	}
}

func TestSortedKeysIsStable(t *testing.T) {
	m := map[string]string{"c": "", "a": "", "b": ""}
	got := sortedKeys(m)
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("sortedKeys returned %d keys, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("sortedKeys()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestHandleDebugParamsDisabledWithoutToken(t *testing.T) {
	t.Setenv(commandTokenEnv, "")
	pm := newControlMonitor()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/debug/params?objtyp=BODY", nil)
	pm.handleDebugParams(rec, req)

	// Read-only, but it discloses the full equipment layout — same gate as /command.
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestHandleDebugParamsRequiresToken(t *testing.T) {
	t.Setenv(commandTokenEnv, "s3cret")
	pm := newControlMonitor()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/debug/params?objtyp=BODY", nil)
	pm.handleDebugParams(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestHandleDebugParamsRequiresObjtyp(t *testing.T) {
	t.Setenv(commandTokenEnv, "s3cret")
	pm := newControlMonitor()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/debug/params", nil)
	req.Header.Set("X-Auth-Token", "s3cret")
	pm.handleDebugParams(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestHandleDebugParamsDisconnected(t *testing.T) {
	t.Setenv(commandTokenEnv, "s3cret")
	pm := newControlMonitor()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/debug/params?objtyp=BODY", nil)
	req.Header.Set("X-Auth-Token", "s3cret")
	pm.handleDebugParams(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}
