package test

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
)

// An upload is the script's as PHP gives it: $_FILES, $_POST and
// move_uploaded_file(), which refuses a file that did not come with it.
func TestClassicUploads(t *testing.T) {
	eachClassic(t, 1, func(t *testing.T, base string) {
		var form bytes.Buffer
		w := multipart.NewWriter(&form)
		_ = w.WriteField("note", "for you")
		part, _ := w.CreateFormFile("doc", "notes.txt")
		_, _ = part.Write([]byte("uploaded text"))
		_ = w.Close()
		resp, err := http.Post(base+"/upload.php", w.FormDataContentType(), &form)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		want := `{"name":"notes.txt","size":13,"field":"for you","content":"uploaded text","foreign":false}`
		if got := strings.TrimSpace(string(body)); got != want {
			t.Errorf("upload.php: %s, want %s", got, want)
		}
	})
}
