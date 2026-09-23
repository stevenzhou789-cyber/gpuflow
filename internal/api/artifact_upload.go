package api

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"path"
	"strings"

	"gpuflow/internal/artifact"
)

const maxArtifactEnvelopeBytes = 1 << 20

var errArtifactTooLarge = errors.New("artifact exceeds the 1 TiB size limit")

type artifactUpload struct {
	name      string
	size      int64
	reader    *artifactUploadReader
	multipart *multipart.Reader
	fields    int64
	body      *artifactRequestBody
	bodySize  int64
}

type artifactRequestBody struct {
	io.ReadCloser
	bytes int64
	err   error
}

func (b *artifactRequestBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.bytes += int64(n)
	if err != nil && err != io.EOF {
		b.err = err
	}
	return n, err
}

// artifactUploadReader caps file bytes independently of the multipart envelope
// and remembers read failures even if a storage SDK swallows UnexpectedEOF.
type artifactUploadReader struct {
	input io.Reader
	bytes int64
	err   error
}

func (r *artifactUploadReader) Read(p []byte) (int, error) {
	remaining := artifact.MaxSize - r.bytes + 1
	if remaining <= 0 {
		return 0, errArtifactTooLarge
	}
	if int64(len(p)) > remaining {
		p = p[:remaining]
	}
	n, err := r.input.Read(p)
	r.bytes += int64(n)
	if r.bytes > artifact.MaxSize {
		err = errArtifactTooLarge
	}
	if err != nil && err != io.EOF {
		r.err = err
	}
	return n, err
}

func readArtifactUpload(w http.ResponseWriter, r *http.Request) (*artifactUpload, error) {
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return nil, fmt.Errorf("invalid artifact Content-Type: %w", err)
	}
	limit := artifact.MaxSize
	if contentType == "multipart/form-data" {
		limit += maxArtifactEnvelopeBytes
	}
	if r.ContentLength > limit {
		return nil, errArtifactTooLarge
	}
	body := &artifactRequestBody{ReadCloser: http.MaxBytesReader(w, r.Body, limit)}
	r.Body = body
	upload := &artifactUpload{size: -1, body: body, bodySize: r.ContentLength}
	var input io.Reader
	switch contentType {
	case "application/octet-stream":
		_, parameters, err := mime.ParseMediaType(r.Header.Get("Content-Disposition"))
		if err != nil {
			return nil, fmt.Errorf("artifact Content-Disposition with filename is required")
		}
		upload.name = parameters["filename"]
		if r.ContentLength < 0 {
			return nil, errors.New("artifact Content-Length is required")
		}
		upload.size = r.ContentLength
		input = r.Body
	case "multipart/form-data":
		upload.multipart, err = r.MultipartReader()
		if err != nil {
			return nil, err
		}
		part, err := upload.nextFile()
		if err != nil {
			return nil, err
		}
		upload.name, input = part.FileName(), part
	default:
		return nil, errors.New("artifact upload requires application/octet-stream or multipart/form-data")
	}
	// Normalize both Windows and POSIX paths identically to object storage.
	upload.name = path.Base(strings.ReplaceAll(upload.name, "\\", "/"))
	if upload.name == "" || upload.name == "." || upload.name == ".." || upload.name == "/" || strings.ContainsAny(upload.name, "\r\n\x00") {
		return nil, errors.New("invalid artifact filename")
	}
	upload.reader = &artifactUploadReader{input: input}
	return upload, nil
}

func (u *artifactUpload) nextFile() (*multipart.Part, error) {
	for i := 0; i < 32; i++ {
		part, err := u.multipart.NextPart()
		if err != nil {
			return nil, err
		}
		if part.FormName() == "file" && part.FileName() != "" {
			return part, nil
		}
		if part.FileName() != "" {
			return nil, errors.New("unexpected file in artifact upload")
		}
		n, err := io.Copy(io.Discard, io.LimitReader(part, maxArtifactEnvelopeBytes-u.fields+1))
		u.fields += n
		if u.fields > maxArtifactEnvelopeBytes {
			return nil, errors.New("artifact multipart fields exceed 1 MiB")
		}
		if err != nil {
			return nil, err
		}
	}
	return nil, errors.New("too many artifact multipart fields")
}

func (u *artifactUpload) finish() error {
	if u.reader.err != nil {
		return u.reader.err
	}
	var trailing [1]byte
	n, err := u.reader.Read(trailing[:])
	if err != nil && err != io.EOF {
		return err
	}
	if n != 0 || err != io.EOF {
		return errors.New("artifact storage did not consume the complete upload")
	}
	if u.size >= 0 && u.reader.bytes != u.size {
		return fmt.Errorf("incomplete artifact: received %d of %d bytes", u.reader.bytes, u.size)
	}
	if u.multipart != nil {
		if _, err := u.nextFile(); err != io.EOF {
			if err != nil {
				return err
			}
			return errors.New("only one artifact file may be uploaded per request")
		}
		// Validate HTTP framing too, including any MIME epilogue after the final
		// boundary. A prematurely closed request must not publish its artifact.
		n, err := io.Copy(io.Discard, io.LimitReader(u.body, maxArtifactEnvelopeBytes+1))
		if err != nil {
			return err
		}
		if n > maxArtifactEnvelopeBytes || u.body.bytes-u.reader.bytes > maxArtifactEnvelopeBytes {
			return errors.New("artifact multipart epilogue exceeds 1 MiB")
		}
	}
	if u.body.err != nil {
		return u.body.err
	}
	if u.bodySize >= 0 && u.body.bytes != u.bodySize {
		return errors.New("incomplete artifact HTTP request body")
	}
	return nil
}

func writeArtifactUploadError(w http.ResponseWriter, err error, fallbackStatus int) {
	var maxBytes *http.MaxBytesError
	if errors.Is(err, errArtifactTooLarge) || errors.As(err, &maxBytes) {
		fallbackStatus = http.StatusRequestEntityTooLarge
	}
	writeError(w, fallbackStatus, err.Error())
}
