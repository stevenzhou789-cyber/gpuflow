package artifact

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

func TestCommitLargeArtifactUsesMultipartCopyAndAbortsFailures(t *testing.T) {
	for _, failPart := range []bool{false, true} {
		t.Run(fmt.Sprintf("fail_part=%v", failPart), func(t *testing.T) {
			var mu sync.Mutex
			var ranges []string
			var completed, aborted bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				w.Header().Set("Content-Type", "application/xml")
				if r.Method == http.MethodPost && r.URL.Query().Has("uploads") {
					fmt.Fprint(w, `<InitiateMultipartUploadResult><Bucket>artifacts</Bucket><Key>job/version/file</Key><UploadId>copy-id</UploadId></InitiateMultipartUploadResult>`)
					return
				}
				if r.URL.Query().Get("uploadId") != "copy-id" {
					t.Errorf("unexpected non-multipart operation: %s %s", r.Method, r.URL)
					http.Error(w, "unexpected operation", http.StatusBadRequest)
					return
				}
				switch r.Method {
				case http.MethodPut:
					ranges = append(ranges, r.Header.Get("X-Amz-Copy-Source-Range"))
					if r.Header.Get("X-Amz-Copy-Source-If-Match") != "staged-etag" {
						t.Errorf("missing source ETag fence: %v", r.Header)
					}
					if failPart && len(ranges) == 2 {
						w.WriteHeader(http.StatusForbidden)
						fmt.Fprint(w, `<Error><Code>AccessDenied</Code><Message>injected part failure</Message></Error>`)
						return
					}
					fmt.Fprintf(w, `<CopyPartResult><ETag>"part-%s"</ETag><LastModified>%s</LastModified></CopyPartResult>`, r.URL.Query().Get("partNumber"), time.Now().UTC().Format(time.RFC3339))
				case http.MethodPost:
					var manifest struct {
						Parts []struct {
							PartNumber int
							ETag       string
						} `xml:"Part"`
					}
					if err := xml.NewDecoder(r.Body).Decode(&manifest); err != nil || len(manifest.Parts) != 2 {
						t.Errorf("multipart manifest: %+v err=%v", manifest, err)
					}
					completed = true
					fmt.Fprint(w, `<CompleteMultipartUploadResult><Bucket>artifacts</Bucket><Key>job/version/file</Key><ETag>"complete"</ETag></CompleteMultipartUploadResult>`)
				case http.MethodDelete:
					aborted = true
					w.WriteHeader(http.StatusNoContent)
				default:
					t.Errorf("unexpected operation: %s", r.Method)
				}
			}))
			defer server.Close()
			client, err := minio.New(strings.TrimPrefix(server.URL, "http://"), &minio.Options{
				Creds: credentials.NewStaticV4("access", "secret", ""), Region: "us-east-1",
			})
			if err != nil {
				t.Fatal(err)
			}
			backend := &minioStore{client: client, bucket: "artifacts"}
			err = backend.Commit(context.Background(), Staged{sourceKey: "job/stage/file", destinationKey: "job/version/file", size: copyPartSize + 123, etag: "staged-etag"})
			if (err != nil) != failPart {
				t.Fatalf("Commit error=%v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			wantRanges := []string{fmt.Sprintf("bytes=0-%d", copyPartSize-1), fmt.Sprintf("bytes=%d-%d", copyPartSize, copyPartSize+122)}
			if !reflect.DeepEqual(ranges, wantRanges) || completed == failPart || aborted != failPart {
				t.Fatalf("ranges=%v completed=%v aborted=%v", ranges, completed, aborted)
			}
		})
	}
}

type brokenLegacyUpload struct{ sent bool }

func (r *brokenLegacyUpload) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		return copy(p, "partial"), nil
	}
	return 0, io.ErrUnexpectedEOF
}

func TestStageDoesNotAcceptInterruptedLegacyBody(t *testing.T) {
	backend := &minioStore{} // failure must be detected before any S3 operation
	if _, err := backend.Stage(context.Background(), "job", "result", &brokenLegacyUpload{}, -1); err != io.ErrUnexpectedEOF {
		t.Fatalf("Stage error = %v", err)
	}
}
