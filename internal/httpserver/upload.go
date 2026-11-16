package httpserver

import (
	"bytes"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"os"
	"strings"
)

// PHP's upload error codes (UPLOAD_ERR_*).
const (
	uploadOK      = 0
	uploadNoFile  = 4
	uploadCantWrt = 7
)

// maxUploads mirrors PHP's max_file_uploads default; further files are dropped.
const maxUploads = 20

// formField is one non-file part, in request order, so PHP can rebuild
// $_POST with its own bracket rules ("tags[]", "a[b]").
type formField [2]string

// uploadedFile is one file part, written to a temporary file that Go removes
// after the request (unless PHP moved it away).
type uploadedFile struct {
	Field    string `json:"field"`
	Name     string `json:"name"`
	FullPath string `json:"full_path"`
	Type     string `json:"type"`
	Size     int64  `json:"size"`
	TmpName  string `json:"tmp_name"`
	Error    int    `json:"error"`
}

type multipartForm struct {
	fields []formField
	files  []uploadedFile
}

// cleanup removes the temporary files that are still there.
func (f *multipartForm) cleanup() {
	for _, u := range f.files {
		if u.TmpName != "" {
			_ = os.Remove(u.TmpName)
		}
	}
}

// parseMultipart reads a multipart/form-data body like PHP's rfc1867 handler:
// fields stay strings, files go to temporary files under dir.
func parseMultipart(body []byte, contentType, dir string) (*multipartForm, error) {
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return nil, err
	}
	boundary := params["boundary"]
	if boundary == "" {
		return nil, errors.New("multipart body without a boundary")
	}
	form := &multipartForm{}
	r := multipart.NewReader(bytes.NewReader(body), boundary)
	for {
		part, err := r.NextRawPart()
		if errors.Is(err, io.EOF) {
			return form, nil
		}
		if err != nil {
			form.cleanup()
			return nil, err
		}
		_, disp, err := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
		name := disp["name"]
		if err != nil || name == "" {
			continue
		}
		filename, isFile := disp["filename"]
		if !isFile {
			v, err := io.ReadAll(part)
			if err != nil {
				form.cleanup()
				return nil, err
			}
			form.fields = append(form.fields, formField{name, string(v)})
			continue
		}
		if len(form.files) >= maxUploads {
			continue
		}
		u := uploadedFile{Field: name, Name: baseName(filename), FullPath: filename, Type: part.Header.Get("Content-Type")}
		if filename == "" {
			u.Error = uploadNoFile
			form.files = append(form.files, u)
			continue
		}
		tmp, err := os.CreateTemp(dir, "cairo-upload-")
		if err != nil {
			u.Error = uploadCantWrt
			form.files = append(form.files, u)
			continue
		}
		u.Size, err = io.Copy(tmp, part)
		if cerr := tmp.Close(); err == nil {
			err = cerr
		}
		u.TmpName = tmp.Name()
		if err != nil {
			u.Error = uploadCantWrt
		}
		form.files = append(form.files, u)
	}
}

// baseName is the file name without any client-supplied directories, as PHP
// reports it in $_FILES[...]['name'].
func baseName(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}
