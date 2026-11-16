package test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Isolated mode, on one PHP thread: requests that keep state in globals run
// one at a time and never see each other's state; echo, header() and
// http_response_code() make the response; superglobals are filled; services
// are reset after each request.
func TestIsolatedMode(t *testing.T) {
	addr := freeAddr()
	cmd := exec.Command(cairoBin, "serve", "../examples/isolated/server.php", "--threads", "1", "--offload-threads", "1")
	cmd.Env = append(os.Environ(), "CAIRO_HTTP="+addr)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Signal(os.Interrupt)
		_ = cmd.Wait()
	}()
	if err := waitFor(addr, 10*time.Second); err != nil {
		t.Fatal(err)
	}

	const n = 5
	start := time.Now()
	var wg sync.WaitGroup
	var mu sync.Mutex
	resets := map[int]bool{}
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			who := fmt.Sprintf("user%d", i)
			resp, err := http.Get("http://" + addr + "/legacy?who=" + who)
			if err != nil {
				t.Error(err)
				return
			}
			defer resp.Body.Close()
			var out struct {
				Who, User, Method string
				Resets            int
			}
			b, _ := io.ReadAll(resp.Body)
			if err := json.Unmarshal(b, &out); err != nil {
				t.Errorf("%s: %v", b, err)
				return
			}
			if resp.StatusCode != 201 || resp.Header.Get("X-Legacy") != "yes" || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
				t.Errorf("status %d headers %v", resp.StatusCode, resp.Header)
			}
			if out.Who != who || out.User != who || out.Method != "GET" {
				t.Errorf("%s saw %+v: state leaked between requests", who, out)
			}
			mu.Lock()
			resets[out.Resets] = true
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	took := time.Since(start)
	t.Logf("%d isolated requests sleeping 0.2s on 1 PHP thread: %s (one at a time)", n, took.Round(time.Millisecond))
	if took < time.Duration(n)*200*time.Millisecond {
		t.Fatalf("took %s; isolated requests overlapped", took)
	}
	if len(resets) != n {
		t.Fatalf("each request should see a different reset count: %v", resets)
	}

	req, _ := http.NewRequest("POST", "http://"+addr+"/form", strings.NewReader("name=Mohamed"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", "session=abc")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "hello Mohamed (abc)" || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("form: %q %v", b, resp.Header)
	}
}

// Multipart uploads in isolated mode: $_POST keeps PHP's bracket rules,
// $_FILES has PHP's layout, move_uploaded_file() moves only real uploads,
// and Go removes the temporary files after the request.
func TestIsolatedUploads(t *testing.T) {
	addr := freeAddr()
	cmd := exec.Command(cairoBin, "serve", "../examples/isolated/server.php", "--threads", "1", "--offload-threads", "1")
	cmd.Env = append(os.Environ(), "CAIRO_HTTP="+addr)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Signal(os.Interrupt)
		_ = cmd.Wait()
	}()
	if err := waitFor(addr, 10*time.Second); err != nil {
		t.Fatal(err)
	}

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("name", "Mohamed")
	_ = mw.WriteField("tags[]", "a")
	_ = mw.WriteField("tags[]", "b")
	_ = mw.WriteField("user[city]", "Cairo")
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="avatar"; filename="dir/me.png"`)
	h.Set("Content-Type", "image/png")
	w, _ := mw.CreatePart(h)
	_, _ = w.Write([]byte("PNGDATA"))
	for _, d := range []string{"first", "second"} {
		w, _ := mw.CreateFormFile("docs[]", d+".txt")
		_, _ = w.Write([]byte(d + " doc"))
	}
	w, _ = mw.CreateFormFile("empty", "")
	_ = mw.Close()

	before, _ := filepath.Glob(filepath.Join(os.TempDir(), "cairo-upload-*"))
	resp, err := http.Post("http://"+addr+"/upload", mw.FormDataContentType(), &body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var out struct {
		Post struct {
			Name string
			Tags []string
			User map[string]string
		}
		Avatar struct {
			Name, Type  string
			Size, Error int
		}
		EmptyError   int      `json:"empty_error"`
		Docs         []string `json:"docs"`
		DocsNames    []string `json:"docs_names"`
		FakeIsUpload bool     `json:"fake_is_upload"`
		FakeMove     bool     `json:"fake_move"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("%s: %v", b, err)
	}
	t.Logf("%s", b)
	if out.Post.Name != "Mohamed" || strings.Join(out.Post.Tags, ",") != "a,b" || out.Post.User["city"] != "Cairo" {
		t.Errorf("$_POST = %+v", out.Post)
	}
	if out.Avatar.Name != "me.png" || out.Avatar.Type != "image/png" || out.Avatar.Size != 7 || out.Avatar.Error != 0 {
		t.Errorf("$_FILES['avatar'] = %+v", out.Avatar)
	}
	if out.EmptyError != 4 {
		t.Errorf("empty file input: error %d, want UPLOAD_ERR_NO_FILE (4)", out.EmptyError)
	}
	if strings.Join(out.Docs, "|") != "first doc|second doc" || strings.Join(out.DocsNames, ",") != "first.txt,second.txt" {
		t.Errorf("docs[] = %v names %v", out.Docs, out.DocsNames)
	}
	if out.FakeIsUpload || out.FakeMove {
		t.Errorf("a file that was not uploaded passed is_uploaded_file/move_uploaded_file")
	}
	after, _ := filepath.Glob(filepath.Join(os.TempDir(), "cairo-upload-*"))
	if len(after) > len(before) {
		t.Errorf("temporary upload files left behind: %v", after)
	}
}
