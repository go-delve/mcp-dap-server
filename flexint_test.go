package main

import (
	"encoding/json"
	"testing"
)

func TestFlexIntUnmarshal(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    int
		wantErr bool
	}{
		{"integer", `42`, 42, false},
		{"string integer", `"42"`, 42, false},
		{"zero", `0`, 0, false},
		{"string zero", `"0"`, 0, false},
		{"negative", `-5`, -5, false},
		{"string negative", `"-5"`, -5, false},
		{"invalid string", `"abc"`, 0, true},
		{"boolean", `true`, 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var f FlexInt
			err := json.Unmarshal([]byte(tt.input), &f)
			if (err != nil) != tt.wantErr {
				t.Errorf("Unmarshal(%s) error = %v, wantErr %v", tt.input, err, tt.wantErr)
				return
			}
			if !tt.wantErr && f.Int() != tt.want {
				t.Errorf("Unmarshal(%s) = %d, want %d", tt.input, f.Int(), tt.want)
			}
		})
	}
}

func TestFlexIntInStruct(t *testing.T) {
	type testParams struct {
		Count FlexInt `json:"count"`
	}

	tests := []struct {
		name  string
		input string
		want  int
	}{
		{"integer field", `{"count": 20}`, 20},
		{"string field", `{"count": "20"}`, 20},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var p testParams
			if err := json.Unmarshal([]byte(tt.input), &p); err != nil {
				t.Fatalf("Unmarshal(%s) error = %v", tt.input, err)
			}
			if p.Count.Int() != tt.want {
				t.Errorf("Count = %d, want %d", p.Count.Int(), tt.want)
			}
		})
	}
}
