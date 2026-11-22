package test

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

// A header is found by its name in any case, and several values are read
// from one query string; a value that is a list is not a string.
func TestRequestHeaderAndQueryLookups(t *testing.T) {
	req, err := http.NewRequest("GET", "http://"+httpAddr+"/lookup?page=2&sort=name&tags[]=a&tags[]=b", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("x-trace-id", "t-1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var got map[string]*string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	want := map[string]string{"canonical": "t-1", "lower": "t-1", "upper": "t-1", "page": "2", "sort": "name", "page_again": "2"}
	for k, v := range want {
		if got[k] == nil || *got[k] != v {
			t.Errorf("%s: got %v, want %q (%s)", k, got[k], v, raw)
		}
	}
	for _, k := range []string{"missing", "list", "absent"} {
		if got[k] != nil {
			t.Errorf("%s: got %q, want null (%s)", k, *got[k], raw)
		}
	}
}
