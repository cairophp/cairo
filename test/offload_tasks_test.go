package test

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestOffloadTaskObjects(t *testing.T) {
	var out map[string]any
	if err := json.Unmarshal([]byte(get(t, "/offload/task?rounds=2000")), &out); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"same_as_local": true,
		"distinct":      true,
		"modes":         []any{"offload", "offload", "offload"},
		"error":         "DomainException: bad input",
	}
	if !reflect.DeepEqual(out, want) {
		t.Fatalf("/offload/task = %v, want %v", out, want)
	}
}
