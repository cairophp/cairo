package test

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPDOPgsqlNotify(t *testing.T) {
	pgsqlEnv(t)
	raw := get(t, "/pgsql/notify?wait=3000&delay=300")
	var out struct {
		Before    any            `json:"before"`
		First     map[string]any `json:"first"`
		Second    []any          `json:"second"`
		SenderPID float64        `json:"sender_pid"`
		Late      struct {
			Payload string `json:"payload"`
			MS      int    `json:"ms"`
		} `json:"late"`
		TimedOut    any `json:"timed_out"`
		StillUsable any `json:"still_usable"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	fail := func(why string) { t.Fatalf("%s: got %s", why, strings.TrimSpace(raw)) }
	if out.Before != false || out.TimedOut != false {
		fail("no notification should give false")
	}
	if out.First["payload"] != "hello" || out.First["pid"] != out.SenderPID || out.SenderPID == 0 || !strings.HasPrefix(out.First["message"].(string), "cairo_") {
		fail("first notification")
	}
	if len(out.Second) != 2 || out.Second[0] != out.First["message"] || out.Second[1] != out.SenderPID {
		fail("second notification (FETCH_NUM, empty payload left out)")
	}
	// The wait suspends the fiber: the task that sends the notification runs
	// meanwhile, and the wait ends when it arrives, not at the timeout.
	if out.Late.Payload != "late" || out.Late.MS < 250 || out.Late.MS > 2500 {
		fail("waiting for a notification")
	}
	if out.StillUsable != float64(1) && out.StillUsable != "1" {
		fail("the connection after a timed-out wait")
	}
}

func TestPDOPgsqlCopy(t *testing.T) {
	pgsqlEnv(t)
	raw := get(t, "/pgsql/copy")
	var out struct {
		In         bool     `json:"in"`
		Semi       bool     `json:"semi"`
		FromFile   bool     `json:"from_file"`
		Rows       []string `json:"rows"`
		Names      []string `json:"names"`
		ToFile     bool     `json:"to_file"`
		File       string   `json:"file"`
		Error      string   `json:"error"`
		Silent     any      `json:"silent"`
		SilentCode string   `json:"silent_code"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	fail := func(why string) { t.Fatalf("%s: got %s", why, strings.TrimSpace(raw)) }
	if !out.In || !out.Semi || !out.FromFile || !out.ToFile {
		fail("copy calls should succeed")
	}
	wantRows := []string{"1\tone\t\\N\n", "2\ttwo\tx\n", "3\tthree\t\\N\n", "4\tfour\t\\N\n"}
	if strings.Join(out.Rows, "") != strings.Join(wantRows, "") || len(out.Rows) != 4 {
		fail("pgsqlCopyToArray")
	}
	if strings.Join(out.Names, "") != "1,nil\n2,x\n3,nil\n4,nil\n" {
		fail("pgsqlCopyToArray with fields, separator and null")
	}
	if out.File != "1|one|\\N\n2|two|x\n3|three|\\N\n4|four|\\N\n" {
		fail("pgsqlCopyToFile")
	}
	if out.Error != "22P02" {
		fail("bad COPY data should raise invalid_text_representation")
	}
	if out.Silent != false || out.SilentCode != "42P01" {
		fail("silent mode")
	}
}

func TestPDOPgsqlLargeObjects(t *testing.T) {
	pgsqlEnv(t)
	raw := get(t, "/pgsql/lob")
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	want := map[string]any{
		"oid_is_numeric": true, "written": float64(100_011), "same": true, "length": float64(100_011),
		"slice": "START", "tail": "656e64", "unlinked": true, "usable": float64(1),
		"invalid": "refused",
	}
	for k, v := range want {
		if out[k] != v {
			t.Errorf("%s = %#v, want %#v", k, out[k], v)
		}
	}
	if m, _ := out["missing"].(string); !strings.Contains(m, "does not exist") {
		t.Errorf("missing: %#v, want a 'does not exist' error", out["missing"])
	}
}
