package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/jtarchie/steps/journal"
)

func TestParseGateSelection(t *testing.T) {
	single := &journal.ParkChoices{
		Kind: "single",
		Options: []journal.ParkOption{
			{Event: "approved", Label: "Ship it"},
			{Event: "rejected", Label: "Abort"},
		},
	}
	multi := &journal.ParkChoices{
		Kind:  "multi",
		Event: "chosen",
		Options: []journal.ParkOption{
			{Value: "auth", Label: "auth"},
			{Value: "billing", Label: "billing"},
			{Value: "search", Label: "search"},
		},
		Min: 1, Max: 2,
	}

	cases := []struct {
		name    string
		choices *journal.ParkChoices
		line    string
		want    *gateAnswer
		wantErr string
	}{
		{"empty leaves parked", single, "", nil, ""},
		{"number picks option", single, "2", &gateAnswer{event: "rejected"}, ""},
		{"event name accepted", single, "approved", &gateAnswer{event: "approved"}, ""},
		{"free-form event passes through", single, "timeout", &gateAnswer{event: "timeout"}, ""},
		{"number out of range", single, "7", nil, "between 1 and 2"},
		{"multi comma list", multi, "1,3", &gateAnswer{event: "chosen", data: map[string]any{"selected": []any{"auth", "search"}}}, ""},
		{"multi with spaces", multi, " 2 , 1 ", &gateAnswer{event: "chosen", data: map[string]any{"selected": []any{"billing", "auth"}}}, ""},
		{"multi all hits max", multi, "all", nil, "at most 2"},
		{"multi below min", multi, ",", nil, "at least 1"},
		{"multi non-number", multi, "auth", nil, "not an option number"},
		{"no options free-form", nil, "approved", &gateAnswer{event: "approved"}, ""},
		{"zero options free-form", &journal.ParkChoices{Kind: "single"}, "go", &gateAnswer{event: "go"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseGateSelection(tc.choices, tc.line)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("answer = %+v, want %+v", got, tc.want)
			}
		})
	}
}
