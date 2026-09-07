package main

// Local control surface — LOCAL FORK ONLY, DO NOT UPSTREAM.
//
// Upstream pentameter is a read-only Prometheus exporter by explicit design. This file adds a
// write path so a Hubitat driver can actuate pool equipment (circuits, solar heat enable, heat
// setpoint) without opening a second connection to the IntelliCenter — the controller has a
// small connection limit and contention between pentameter and Homebridge's IntelliCenter
// plugin has been a recurring, already-observed failure. Reusing the WebSocket that is already
// open for RequestParamList polling adds no connections at all.
//
// The endpoint is disabled unless PENTAMETER_COMMAND_TOKEN is set: control is opt-in, so a
// stock deployment behaves exactly as upstream does.

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	// commandTokenEnv gates the whole control surface. Unset means /command is not served.
	commandTokenEnv = "PENTAMETER_COMMAND_TOKEN"

	// heatSourceOff is IntelliCenter's sentinel for "no heater assigned to this body".
	// Writing it to HTSRC disables heat; writing a heater objnam enables it.
	heatSourceOff = "00000"

	// Setpoint bounds. IntelliCenter itself accepts a wider range than is ever sensible for a
	// pool, and a bad value written over the wire is a physical-equipment problem, so reject
	// out-of-range requests here rather than passing them through.
	minSetpointF = 40
	maxSetpointF = 104

	// commandBodyLimit caps the request body. Commands are a few dozen bytes; anything larger
	// is a mistake or an attack, not a real command.
	commandBodyLimit = 4096
)

// ObjectSet is the write counterpart to ObjectQuery. IntelliCenter's SetParamList takes
// {objnam, params: {...}} where GetParamList takes {objnam, keys: [...]}, so this cannot reuse
// ObjectQuery — a request carrying an empty "keys" array is rejected by the controller.
type ObjectSet struct {
	Params  map[string]string `json:"params"`
	ObjName string            `json:"objnam"`
}

// IntelliCenterSetRequest is a SetParamList message.
type IntelliCenterSetRequest struct {
	MessageID  string      `json:"messageID"`
	Command    string      `json:"command"`
	ObjectList []ObjectSet `json:"objectList"`
}

// CommandRequest is the JSON body accepted by POST /command.
//
// Exactly one action is performed per request:
//
//	{"circuit": "C0006", "state": "off"}   — switch a circuit
//	{"circuit": "Pool Light", "state": "on"} — same, resolved by name
//	{"body": "pool", "heat": "on"}          — assign/clear the body's heat source
//	{"body": "pool", "setpoint": 82}        — set the body's heat setpoint (LOTMP)
type CommandRequest struct {
	Setpoint *float64 `json:"setpoint,omitempty"`
	Circuit  string   `json:"circuit,omitempty"`
	State    string   `json:"state,omitempty"`
	Body     string   `json:"body,omitempty"`
	Heat     string   `json:"heat,omitempty"`
	Heater   string   `json:"heater,omitempty"`
}

// CommandResponse is returned for both success and failure so a client can always parse one
// shape.
type CommandResponse struct {
	Status  string `json:"status"`
	Applied string `json:"applied,omitempty"`
	Error   string `json:"error,omitempty"`
}

// errNotConnected is returned when a command arrives while the IntelliCenter link is down.
var errNotConnected = errors.New("not connected to IntelliCenter")

// registerBodyObject records a body's objnam under both its friendly name and its subtype, so
// callers can say "pool" without knowing that the controller calls it B1101. Called on every
// body poll, unconditionally — see the note at its call site for why referencedHeaters is not
// a safe source for this.
func (pm *PoolMonitor) registerBodyObject(name, subtype, objName string) {
	if objName == "" {
		return
	}
	if name != "" {
		pm.bodyObjects[strings.ToLower(name)] = objName
	}
	if subtype != "" {
		pm.bodyObjects[strings.ToLower(subtype)] = objName
	}
	pm.bodyObjects[strings.ToLower(objName)] = objName
}

// resolveBody maps a caller-supplied body reference to an objnam.
func (pm *PoolMonitor) resolveBody(ref string) (string, error) {
	if ref == "" {
		return "", errors.New("body is required")
	}
	if objName, ok := pm.bodyObjects[strings.ToLower(ref)]; ok {
		return objName, nil
	}
	return "", fmt.Errorf("unknown body %q (known: %s)", ref, strings.Join(sortedKeys(pm.bodyObjects), ", "))
}

// resolveCircuit maps a caller-supplied circuit reference to an objnam, accepting either the
// objnam itself or the friendly SNAME that pentameter already tracks for metric labels.
func (pm *PoolMonitor) resolveCircuit(ref string) (string, error) {
	if ref == "" {
		return "", errors.New("circuit is required")
	}
	if _, ok := pm.circuitNames[ref]; ok {
		return ref, nil
	}
	lower := strings.ToLower(ref)
	for objName, name := range pm.circuitNames {
		if strings.ToLower(name) == lower {
			return objName, nil
		}
	}
	return "", fmt.Errorf("unknown circuit %q", ref)
}

// resolveHeater picks the heater objnam to assign when enabling heat. With a single installed
// heater the caller can omit it; with more than one, requiring an explicit choice is better
// than silently picking whichever the map iterates first.
func (pm *PoolMonitor) resolveHeater(ref string) (string, error) {
	if ref != "" {
		if _, ok := pm.heaterObjects[ref]; ok {
			return ref, nil
		}
		lower := strings.ToLower(ref)
		for objName, name := range pm.heaterObjects {
			if strings.ToLower(name) == lower {
				return objName, nil
			}
		}
		return "", fmt.Errorf("unknown heater %q", ref)
	}

	switch len(pm.heaterObjects) {
	case 0:
		return "", errors.New("no heaters known yet — wait for the first poll to complete")
	case 1:
		for objName := range pm.heaterObjects {
			return objName, nil
		}
	}
	return "", fmt.Errorf("multiple heaters present (%s) — specify \"heater\"",
		strings.Join(sortedKeys(pm.heaterObjects), ", "))
}

// sendSetParamList writes a SetParamList over the already-open polling WebSocket and waits for
// its acknowledgement. The caller must hold pm.connMu.
func (pm *PoolMonitor) sendSetParamList(objName string, params map[string]string) error {
	if pm.conn == nil || !pm.connected {
		return errNotConnected
	}

	messageID := fmt.Sprintf("set-%d-%d", time.Now().Unix(), time.Now().Nanosecond()%nanosecondMod)
	req := IntelliCenterSetRequest{
		MessageID:  messageID,
		Command:    "SetParamList",
		ObjectList: []ObjectSet{{ObjName: objName, Params: params}},
	}

	pm.pendingRequests[messageID] = time.Now()
	if err := pm.conn.WriteJSON(req); err != nil {
		delete(pm.pendingRequests, messageID)
		return fmt.Errorf("failed to send command: %w", err)
	}

	resp, err := pm.readResponseWithPushHandling(messageID)
	if err != nil {
		delete(pm.pendingRequests, messageID)
		return fmt.Errorf("failed to read command response: %w", err)
	}
	pm.validateResponse(messageID)

	if resp.Response != "200" {
		// Include the controller's own description. Per the protocol notes in API.md,
		// 200=success, 400=bad request, 404=unknown command — but the numeric code
		// alone does not say WHICH command or parameter was not understood, and
		// guessing at that against live equipment is not acceptable.
		if resp.Description != "" {
			return fmt.Errorf("IntelliCenter rejected command: %s (response %s)", resp.Description, resp.Response)
		}
		return fmt.Errorf("IntelliCenter rejected command with response %s", resp.Response)
	}
	return nil
}

// handleDebugParams serves GET /debug/params, a read-only raw dump of whatever the
// controller reports for a given object type. It exists because the write side had
// to be reverse-engineered: knowing that a SetParamList was rejected is useless
// without seeing what the object actually looks like and which parameters it holds.
//
// Deliberately generic — objtyp and keys are both caller-supplied — so the shape of
// a question can change without rebuilding and redeploying the container.
//
//	GET /debug/params?objtyp=BODY&keys=SNAME,SUBTYP,HTSRC,HTMODE,LOTMP,HITMP
//
// Gated by the same token as /command. It is read-only, but it discloses the full
// equipment layout and should not be more open than the control path.
func (pm *PoolMonitor) handleDebugParams(w http.ResponseWriter, r *http.Request) {
	token := getEnvOrDefault(commandTokenEnv, "")
	if token == "" {
		http.NotFound(w, r)
		return
	}
	if provided := r.Header.Get("X-Auth-Token"); len(provided) != len(token) || provided != token {
		writeCommandResponse(w, http.StatusUnauthorized, CommandResponse{
			Status: "error", Error: "invalid or missing X-Auth-Token",
		})
		return
	}

	objtyp := r.URL.Query().Get("objtyp")
	if objtyp == "" {
		writeCommandResponse(w, http.StatusBadRequest, CommandResponse{
			Status: "error", Error: "objtyp is required (e.g. BODY, HEATER, CIRCUIT)",
		})
		return
	}

	keys := defaultDebugKeys
	if raw := r.URL.Query().Get("keys"); raw != "" {
		keys = nil
		for _, k := range strings.Split(raw, ",") {
			if k = strings.TrimSpace(k); k != "" {
				keys = append(keys, k)
			}
		}
	}

	pm.connMu.Lock()
	resp, err := pm.queryParams(objtyp, keys)
	pm.connMu.Unlock()

	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, errNotConnected) {
			status = http.StatusServiceUnavailable
		}
		writeCommandResponse(w, status, CommandResponse{Status: "error", Error: err.Error()})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("Failed to write debug response: %v", err)
	}
}

// defaultDebugKeys is a broad superset covering the parameters seen on bodies,
// heaters and circuits. The controller ignores keys an object does not have, so
// over-asking is harmless and saves a round trip of guessing.
var defaultDebugKeys = []string{
	"SNAME", "OBJTYP", "SUBTYP", "STATUS", "MODE",
	"TEMP", "LOTMP", "HITMP", "LSTTMP",
	"HTMODE", "HTSRC", "HEATER", "BODY",
	"COOL", "PRIM", "SEC", "ACT", "VOL", "MANUL", "LISTORD", "SHOMNU",
}

// queryParams runs a GetParamList for one object type. The caller must hold connMu.
func (pm *PoolMonitor) queryParams(objtyp string, keys []string) (*IntelliCenterResponse, error) {
	if pm.conn == nil || !pm.connected {
		return nil, errNotConnected
	}

	messageID := fmt.Sprintf("debug-%d-%d", time.Now().Unix(), time.Now().Nanosecond()%nanosecondMod)
	req := IntelliCenterRequest{
		MessageID:  messageID,
		Command:    "GetParamList",
		Condition:  "OBJTYP=" + objtyp,
		ObjectList: []ObjectQuery{{ObjName: "INCR", Keys: keys}},
	}

	pm.pendingRequests[messageID] = time.Now()
	if err := pm.conn.WriteJSON(req); err != nil {
		delete(pm.pendingRequests, messageID)
		return nil, fmt.Errorf("failed to send debug query: %w", err)
	}

	resp, err := pm.readResponseWithPushHandling(messageID)
	if err != nil {
		delete(pm.pendingRequests, messageID)
		return nil, fmt.Errorf("failed to read debug response: %w", err)
	}
	pm.validateResponse(messageID)
	return resp, nil
}

// applyCommand validates a request and performs exactly one action. The caller must hold
// pm.connMu.
func (pm *PoolMonitor) applyCommand(req *CommandRequest) (string, error) {
	actions := 0
	if req.Circuit != "" {
		actions++
	}
	if req.Heat != "" {
		actions++
	}
	if req.Setpoint != nil {
		actions++
	}

	switch actions {
	case 0:
		return "", errors.New(`no action given: expected one of "circuit"+"state", "heat", or "setpoint"`)
	case 1:
	default:
		return "", errors.New("give exactly one action per request")
	}

	switch {
	case req.Circuit != "":
		return pm.applyCircuit(req)
	case req.Heat != "":
		return pm.applyHeat(req)
	default:
		return pm.applySetpoint(req)
	}
}

func (pm *PoolMonitor) applyCircuit(req *CommandRequest) (string, error) {
	state, err := normalizeOnOff(req.State, "state")
	if err != nil {
		return "", err
	}
	objName, err := pm.resolveCircuit(req.Circuit)
	if err != nil {
		return "", err
	}
	if err := pm.sendSetParamList(objName, map[string]string{"STATUS": state}); err != nil {
		return "", err
	}
	return fmt.Sprintf("circuit %s (%s) -> %s", objName, pm.circuitNames[objName], state), nil
}

func (pm *PoolMonitor) applyHeat(req *CommandRequest) (string, error) {
	state, err := normalizeOnOff(req.Heat, "heat")
	if err != nil {
		return "", err
	}
	bodyObj, err := pm.resolveBody(req.Body)
	if err != nil {
		return "", err
	}

	// Heat is enabled by assigning a heater to the body and disabled by clearing that
	// assignment — the heater object itself has no on/off of its own.
	source := heatSourceOff
	if state == "ON" {
		source, err = pm.resolveHeater(req.Heater)
		if err != nil {
			return "", err
		}
	}

	// Write HEATER, not HTSRC. The body carries both, and both report the assigned
	// heater objnam, but only HEATER is settable: a SetParamList writing HTSRC on
	// this same object is rejected with response 404 (and no description), while
	// LOTMP on the same object succeeds — so the controller is refusing that
	// specific parameter, not the command or the object. HTSRC is the reported
	// heat source; HEATER is the assignment. Confirmed against the live controller
	// via GET /debug/params?objtyp=BODY.
	if err := pm.sendSetParamList(bodyObj, map[string]string{"HEATER": source}); err != nil {
		return "", err
	}
	return fmt.Sprintf("body %s heat -> %s (HEATER=%s)", bodyObj, strings.ToLower(state), source), nil
}

func (pm *PoolMonitor) applySetpoint(req *CommandRequest) (string, error) {
	setpoint := *req.Setpoint
	if setpoint < minSetpointF || setpoint > maxSetpointF {
		return "", fmt.Errorf("setpoint %.1f out of range (%d-%d°F)", setpoint, minSetpointF, maxSetpointF)
	}
	bodyObj, err := pm.resolveBody(req.Body)
	if err != nil {
		return "", err
	}

	value := strconv.Itoa(int(setpoint))
	if err := pm.sendSetParamList(bodyObj, map[string]string{"LOTMP": value}); err != nil {
		return "", err
	}
	return fmt.Sprintf("body %s setpoint -> %s°F (LOTMP)", bodyObj, value), nil
}

// normalizeOnOff accepts the spellings a driver or a curl one-liner is likely to send and
// returns IntelliCenter's own representation.
func normalizeOnOff(value, field string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "on", "1", trueString:
		return "ON", nil
	case "off", "0", "false":
		return "OFF", nil
	default:
		return "", fmt.Errorf("%s must be on or off, got %q", field, value)
	}
}

// handleCommand serves POST /command.
func (pm *PoolMonitor) handleCommand(w http.ResponseWriter, r *http.Request) {
	token := getEnvOrDefault(commandTokenEnv, "")
	if token == "" {
		// Control is opt-in. Without a token there is nothing to authenticate against, so the
		// route behaves as if it does not exist rather than running unauthenticated.
		http.NotFound(w, r)
		return
	}

	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeCommandResponse(w, http.StatusMethodNotAllowed, CommandResponse{
			Status: "error", Error: "POST required",
		})
		return
	}

	// Constant-time comparison is not warranted here: the token is a LAN-only shared secret and
	// the surrounding network is the real boundary. Length check first so a short token cannot
	// be probed by timing the compare.
	if provided := r.Header.Get("X-Auth-Token"); len(provided) != len(token) || provided != token {
		writeCommandResponse(w, http.StatusUnauthorized, CommandResponse{
			Status: "error", Error: "invalid or missing X-Auth-Token",
		})
		return
	}

	var req CommandRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, commandBodyLimit)).Decode(&req); err != nil {
		writeCommandResponse(w, http.StatusBadRequest, CommandResponse{
			Status: "error", Error: fmt.Sprintf("invalid JSON body: %v", err),
		})
		return
	}

	// Serialize against the polling loop for the whole send-and-await cycle.
	pm.connMu.Lock()
	applied, err := pm.applyCommand(&req)
	pm.connMu.Unlock()

	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errNotConnected) {
			status = http.StatusServiceUnavailable
		}
		log.Printf("Command rejected: %v", err)
		writeCommandResponse(w, status, CommandResponse{Status: "error", Error: err.Error()})
		return
	}

	log.Printf("Command applied: %s", applied)
	writeCommandResponse(w, http.StatusOK, CommandResponse{Status: "ok", Applied: applied})
}

func writeCommandResponse(w http.ResponseWriter, status int, resp CommandResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("Failed to write command response: %v", err)
	}
}

// sortedKeys returns a map's keys in a stable order, so error messages listing valid values do
// not shuffle between calls.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
