package main

// Manual triggering of served machines from the web UI. A machine registered
// with --machine (or --hook, which is manually triggerable too) gets a
// /machines/<name> page whose form is generated from its input: block: one
// text field plus a file-upload alternative per declared input, and a raw-JSON
// escape hatch for undeclared keys. A submission rides the same durable path as
// a webhook — Enqueue + dispatcher — so it survives restarts, is bounded by the
// machine's maxInFlight/maxQueued, and 429s when the queue is full.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime/multipart"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/labstack/echo/v5"

	"github.com/jtarchie/steps/machine"
)

// maxTriggerBody caps a trigger POST (form fields + uploads). A var, not a
// const, so tests can shrink it; promotable to a flag later.
var maxTriggerBody int64 = 10 << 20 // 10 MB

// inputField is one declared input rendered as a form field.
type inputField struct {
	Name     string
	Type     string
	Required bool
}

// machineCard is one machine on the /machines index.
type machineCard struct {
	Name        string
	Description string
	Fields      []inputField
	HookPath    string // "" when the machine has no webhook: block
}

// inputFields lists a machine's declared inputs, sorted by name.
func inputFields(m *machine.Machine) []inputField {
	fields := make([]inputField, 0, len(m.Input))
	for name, spec := range m.Input {
		fields = append(fields, inputField{Name: name, Type: spec.Type, Required: spec.Required})
	}
	sort.Slice(fields, func(i, j int) bool { return fields[i].Name < fields[j].Name })
	return fields
}

// machineNames lists every registered machine, sorted — the Trigger chips on
// the runs page and the /machines index both use it.
func (s *server) machineNames() []string {
	names := make([]string, 0, len(s.machines))
	for name := range s.machines {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// machineByParam resolves the :name path param to a registered machine, trying
// the raw param first and then its percent-decoded form (machine names are not
// constrained to be URL-safe). Returns nil when unregistered.
func (s *server) machineByParam(c *echo.Context) *servedMachine {
	raw := c.Param("name")
	if sm, ok := s.machines[raw]; ok {
		return sm
	}
	dec, err := url.PathUnescape(raw)
	if err == nil {
		if sm, ok := s.machines[dec]; ok {
			return sm
		}
	}
	return nil
}

// handleMachines renders the index of triggerable machines.
func (s *server) handleMachines(c *echo.Context) error {
	cards := make([]machineCard, 0, len(s.machines))
	for _, name := range s.machineNames() {
		sm := s.machines[name]
		card := machineCard{Name: name, Description: sm.m.Description, Fields: inputFields(sm.m)}
		if sm.m.Webhook != nil {
			card.HookPath = sm.m.Webhook.Path
		}
		cards = append(cards, card)
	}
	return s.render(c, http.StatusOK, "machines.html", map[string]any{
		"Title":    "steps — machines",
		"Machines": cards,
	})
}

// handleMachineForm renders the trigger form for one machine.
func (s *server) handleMachineForm(c *echo.Context) error {
	sm := s.machineByParam(c)
	if sm == nil {
		return echo.NewHTTPError(http.StatusNotFound, "no such machine")
	}
	return s.renderMachineForm(c, http.StatusOK, sm, "", nil, "")
}

// renderMachineForm renders machine.html, optionally with an error banner and
// the values the operator already entered (so a validation failure doesn't wipe
// the form).
func (s *server) renderMachineForm(c *echo.Context, code int, sm *servedMachine, errMsg string, values map[string]string, extras string) error {
	fields := inputFields(sm.m)
	return s.render(c, code, "machine.html", map[string]any{
		"Title":       "steps — " + sm.m.Name,
		"Name":        sm.m.Name,
		"Description": sm.m.Description,
		"Fields":      fields,
		"HasInputs":   len(fields) > 0,
		"Error":       errMsg,
		"Values":      values,
		"ExtrasValue": extras,
	})
}

// handleTrigger parses the trigger form into run inputs and durably enqueues a
// run, mirroring handleHook. Precedence, lowest to highest: --hook-input base <
// extras JSON < declared form fields; within a declared field an upload beats a
// typed value (uploading is the more deliberate act). No unknown-key filtering
// — this mirrors `steps run --input`, and it's what makes the extras textarea a
// real escape hatch. A double-POST just enqueues two runs, bounded by maxQueued.
func (s *server) handleTrigger(c *echo.Context) error {
	ctx := c.Request().Context()
	sm := s.machineByParam(c)
	if sm == nil {
		return echo.NewHTTPError(http.StatusNotFound, "no such machine")
	}

	req := c.Request()
	req.Body = http.MaxBytesReader(c.Response(), req.Body, maxTriggerBody)
	herr := parseTriggerBody(req)
	if herr != nil {
		return herr
	}

	input, values, formErr, herr := buildTriggerInput(req, sm)
	if herr != nil {
		return herr
	}
	if formErr != "" {
		return s.renderMachineForm(c, http.StatusBadRequest, sm, formErr, values,
			strings.TrimSpace(req.FormValue("extras_json")))
	}

	herr = s.checkQueueDepth(ctx, sm)
	if herr != nil {
		return herr
	}

	runID, err := s.eng.Enqueue(ctx, sm.m, input, "web")
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "enqueuing run: "+err.Error())
	}
	if s.disp != nil {
		s.disp.poke()
	}

	err = c.Redirect(http.StatusSeeOther, "/runs/"+runID)
	if err != nil {
		return fmt.Errorf("redirecting to run %s: %w", runID, err)
	}
	return nil
}

// parseTriggerBody parses the trigger POST, tolerating a plain urlencoded body
// (no uploads) and turning an over-cap upload into a 413.
func parseTriggerBody(req *http.Request) *echo.HTTPError {
	err := req.ParseMultipartForm(4 << 20)
	if err == nil {
		return nil
	}
	var mbe *http.MaxBytesError
	switch {
	case errors.As(err, &mbe):
		return echo.NewHTTPError(http.StatusRequestEntityTooLarge,
			fmt.Sprintf("upload too large (limit %d MB)", maxTriggerBody>>20))
	case errors.Is(err, http.ErrNotMultipart):
		err = req.ParseForm()
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "malformed form: "+err.Error())
		}
		return nil
	default:
		return echo.NewHTTPError(http.StatusBadRequest, "malformed form: "+err.Error())
	}
}

// buildTriggerInput assembles run inputs from the parsed form. Precedence,
// lowest to highest: --hook-input base < extras JSON < declared form fields;
// within a declared field an upload beats a typed value. It returns the entered
// text values (for a lossless re-render), a friendly form-level error message
// (empty = ok), or an *echo.HTTPError for lower-level failures.
func buildTriggerInput(req *http.Request, sm *servedMachine) (map[string]any, map[string]string, string, *echo.HTTPError) {
	// Capture typed values first so any validation failure re-renders losslessly.
	values := map[string]string{}
	for _, f := range inputFields(sm.m) {
		if v := req.FormValue("in_" + f.Name); v != "" {
			values[f.Name] = v
		}
	}

	input := maps.Clone(sm.inputs)
	if input == nil {
		input = map[string]any{}
	}

	extras, formErr := parseExtras(req.FormValue("extras_json"))
	if formErr != "" {
		return nil, values, formErr, nil
	}
	for k, v := range extras {
		input[k] = v
	}

	herr := applyDeclaredFields(req, sm, input, values)
	if herr != nil {
		return nil, values, "", herr
	}

	missing := missingRequired(sm, input)
	if len(missing) > 0 {
		return nil, values, "missing required input(s): " + strings.Join(missing, ", "), nil
	}
	return input, values, "", nil
}

// parseExtras decodes the raw extras textarea into an inputs object, returning
// a friendly message (empty = ok) instead of a raw error. A blank textarea is
// fine — it just yields no extras.
func parseExtras(raw string) (map[string]any, string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, ""
	}
	extras := map[string]any{}
	err := json.Unmarshal([]byte(raw), &extras)
	if err != nil {
		return nil, "extra inputs must be a JSON object: " + err.Error()
	}
	return extras, ""
}

// applyDeclaredFields overlays each declared input from its upload (preferred)
// or typed value onto input.
func applyDeclaredFields(req *http.Request, sm *servedMachine, input map[string]any, values map[string]string) *echo.HTTPError {
	for _, f := range inputFields(sm.m) {
		fh, ok := formFile(req, "file_"+f.Name)
		if ok && fh.Size > 0 {
			content, err := readUpload(fh)
			if err != nil {
				return echo.NewHTTPError(http.StatusBadRequest, "reading upload "+f.Name+": "+err.Error())
			}
			input[f.Name] = content
			continue
		}
		if v := values[f.Name]; v != "" {
			input[f.Name] = v
		}
	}
	return nil
}

// missingRequired names the declared-required inputs absent from input, sorted.
func missingRequired(sm *servedMachine, input map[string]any) []string {
	var missing []string
	for name, spec := range sm.m.Input {
		if _, ok := input[name]; !ok && spec.Required {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	return missing
}

// formFile returns the first uploaded file for a multipart field, if any.
func formFile(r *http.Request, field string) (*multipart.FileHeader, bool) {
	if r.MultipartForm == nil || r.MultipartForm.File == nil {
		return nil, false
	}
	fhs := r.MultipartForm.File[field]
	if len(fhs) == 0 {
		return nil, false
	}
	return fhs[0], true
}

// readUpload reads an uploaded file's bytes as a string — the same "file
// content becomes the input value" semantics as the CLI's --input key=@file.
func readUpload(fh *multipart.FileHeader) (string, error) {
	f, err := fh.Open()
	if err != nil {
		return "", fmt.Errorf("opening upload: %w", err)
	}
	defer f.Close()
	raw, err := io.ReadAll(f)
	if err != nil {
		return "", fmt.Errorf("reading upload: %w", err)
	}
	return string(raw), nil
}
